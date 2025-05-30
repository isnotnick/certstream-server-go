package certificatetransparency

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/d-Rickyy-b/certstream-server-go/internal/certstream"
	"github.com/d-Rickyy-b/certstream-server-go/internal/config"
	"github.com/d-Rickyy-b/certstream-server-go/internal/web"

	ct "github.com/google/certificate-transparency-go"
	"github.com/google/certificate-transparency-go/client"
	"github.com/google/certificate-transparency-go/jsonclient"
	"github.com/google/certificate-transparency-go/loglist3"
	"github.com/google/certificate-transparency-go/scanner"
)

var (
	errCreatingClient    = errors.New("failed to create JSON client")
	errFetchingSTHFailed = errors.New("failed to fetch STH")
	userAgent            = fmt.Sprintf("Certstream Server v%s (github.com/d-Rickyy-b/certstream-server-go)", config.Version)
	CAOwners             = make(map[string]string)
)

// Watcher describes a component that watches for new certificates in a CT log.
type Watcher struct {
	workers    []*worker
	wg         sync.WaitGroup
	context    context.Context
	certChan   chan certstream.Entry
	cancelFunc context.CancelFunc
}

// NewWatcher creates a new Watcher.
func NewWatcher(certChan chan certstream.Entry) *Watcher {
	return &Watcher{
		certChan: certChan,
	}
}

// Start starts the watcher. This method is blocking.
func (w *Watcher) Start() {
	w.context, w.cancelFunc = context.WithCancel(context.Background())

	// Create new certChan if it doesn't exist yet
	if w.certChan == nil {
		w.certChan = make(chan certstream.Entry, 5000)
	}

	// initialize the watcher with currently available logs
	w.addNewlyAvailableLogs()

	log.Println("Started CT watcher")
	go certHandler(w.certChan)
	go w.watchNewLogs()

	w.wg.Wait()
	close(w.certChan)
}

// watchNewLogs monitors the ct log list for new logs and starts a worker for each new log found.
// This method is blocking. It can be stopped by cancelling the context.
func (w *Watcher) watchNewLogs() {
	// Add all available logs to the watcher
	//w.addNewlyAvailableLogs()

	// Check for new logs once every hour
	//	EDIT - do it ever 6 hours
	ticker := time.NewTicker(6 * time.Hour)
	for {
		select {
		case <-ticker.C:
			w.addNewlyAvailableLogs()
		case <-w.context.Done():
			ticker.Stop()
			return
		}
	}
}

// The transparency log list is constantly updated with new Log servers.
// This function checks for new ct logs and adds them to the watcher.
//
//	ADDED: This will load a list of all the 'trusted' CAs from CCADB, parse the AKIs and 'ca owners' into a map.
func (w *Watcher) addNewlyAvailableLogs() {
	log.Println("Checking for new cas from ccadb...")
	ccadbURL := "https://ccadb.my.salesforce-sites.com/ccadb/AllCertificateRecordsCSVFormatv2"

	//	Download and parse the CSV - the columns we want in the map are 1 - the 'CA Owner' and 19 - SKI. Which is b64-encoded-hex.
	CAOwners, _ = DownloadAndParseCSV(ccadbURL, 18, 0, true)

	log.Printf("Got ccadb file - loaded %v icas...\n", len(CAOwners))

	log.Println("Checking for new ct logs...")

	// Get a list of urls of all CT logs
	logList, err := getAllLogs()
	if err != nil {
		log.Println(err)
		return
	}

	newCTs := 0

	// Check the ct log list for new, unwatched logs
	// For each CT log, create a worker and start downloading certs
	for _, operator := range logList.Operators {
		// Iterate over each log of the operator
		for _, transparencyLog := range operator.Logs {
			// Check if the log is already being watched
			newURL := normalizeCtlogURL(transparencyLog.URL)

			alreadyWatched := false
			for _, ctWorker := range w.workers {
				workerURL := normalizeCtlogURL(ctWorker.ctURL)
				if workerURL == newURL {
					alreadyWatched = true
					break
				}
			}

			// TODO maybe add a check for logs that are still watched but no longer on the logList and remove them? See also issue #41 and #42

			// If the log is not being watched, create a new worker
			if !alreadyWatched {
				w.wg.Add(1)
				newCTs++

				ctWorker := worker{
					name:         transparencyLog.Description,
					operatorName: operator.Name,
					ctURL:        transparencyLog.URL,
					entryChan:    w.certChan,
				}
				w.workers = append(w.workers, &ctWorker)

				// Start a goroutine for each worker
				go func() {
					defer w.wg.Done()
					ctWorker.startDownloadingCerts(w.context)
				}()
			}
		}
	}

	log.Printf("New ct logs found: %d\n", newCTs)
	log.Printf("Currently monitored ct logs: %d\n", len(w.workers))
}

// Stop stops the watcher.
func (w *Watcher) Stop() {
	log.Printf("Stopping watcher\n")
	w.cancelFunc()
}

// A worker processes a single CT log.
type worker struct {
	name         string
	operatorName string
	ctURL        string
	entryChan    chan certstream.Entry
	mu           sync.Mutex
	running      bool
}

// startDownloadingCerts starts downloading certificates from the CT log. This method is blocking.
func (w *worker) startDownloadingCerts(ctx context.Context) {
	// Normalize CT URL. We remove trailing slashes and prepend "https://" if it's not already there.
	w.ctURL = strings.TrimRight(w.ctURL, "/")
	if !strings.HasPrefix(w.ctURL, "https://") && !strings.HasPrefix(w.ctURL, "http://") {
		w.ctURL = "https://" + w.ctURL
	}

	log.Printf("Starting worker for CT log: %s\n", w.ctURL)
	defer log.Printf("Stopping worker for CT log: %s\n", w.ctURL)

	w.mu.Lock()
	if w.running {
		log.Printf("Worker for '%s' already running\n", w.ctURL)
		w.mu.Unlock()

		return
	}

	w.running = true
	w.mu.Unlock()

	for {
		workerErr := w.runWorker(ctx)
		if workerErr != nil {
			if errors.Is(workerErr, errFetchingSTHFailed) {
				log.Printf("Worker for '%s' failed - could not fetch STH\n", w.ctURL)
				return
			} else if errors.Is(workerErr, errCreatingClient) {
				log.Printf("Worker for '%s' failed - could not create client\n", w.ctURL)
				return
			} else if strings.Contains(workerErr.Error(), "no such host") {
				log.Printf("Worker for '%s' failed to resolve host: %s\n", w.ctURL, workerErr)
				return
			}

			log.Printf("Worker for '%s' failed with unexpected error: %s\n", w.ctURL, workerErr)
		}

		// Check if the context was cancelled
		select {
		case <-ctx.Done():
			log.Printf("Context was cancelled; Stopping worker for '%s'\n", w.ctURL)
			return
		default:
			log.Printf("Worker for '%s' sleeping for 5 seconds due to error\n", w.ctURL)
			time.Sleep(5 * time.Second)
			log.Printf("Restarting worker for '%s'\n", w.ctURL)
			continue
		}
	}
}

// runWorker runs a single worker for a single CT log. This method is blocking.
func (w *worker) runWorker(ctx context.Context) error {
	hc := http.Client{Timeout: 30 * time.Second}
	jsonClient, e := client.New(w.ctURL, &hc, jsonclient.Options{UserAgent: userAgent})
	if e != nil {
		log.Printf("Error creating JSON client: %s\n", e)
		return errCreatingClient
	}

	sth, getSTHerr := jsonClient.GetSTH(ctx)
	if getSTHerr != nil {
		log.Printf("Could not get STH for '%s': %s\n", w.ctURL, getSTHerr)
		return errFetchingSTHFailed
	}

	//	Check if the log is in the config file with a specific index to start at. If so, use it (checking it's bigger than 0 and smaller than the current tree size!)
	logStart := int64(sth.TreeSize)

	for _, element := range config.AppConfig.CTLogs.StartIndex {
		if strings.Contains(w.ctURL, element) {
			logStartIndex := strings.Split(element, " ")
			newStartIndex, _ := strconv.Atoi(logStartIndex[1])
			if newStartIndex > 0 {
				logStart = int64(newStartIndex)
			}
		}
	}

	certScanner := scanner.NewScanner(jsonClient, scanner.ScannerOptions{
		FetcherOptions: scanner.FetcherOptions{
			BatchSize:     100,
			ParallelFetch: 1,
			StartIndex:    logStart, // Start at the latest STH to skip all the past certificates
			Continuous:    true,
		},
		Matcher:     scanner.MatchAll{},
		PrecertOnly: false,
		NumWorkers:  1,
		BufferSize:  1000,
	})

	scanErr := certScanner.Scan(ctx, w.foundCertCallback, w.foundPrecertCallback)
	if scanErr != nil {
		log.Println("Scan error: ", scanErr)
		return scanErr
	}

	log.Println("No error from certScanner!")

	return nil
}

// foundCertCallback is the callback that handles cases where new regular certs are found.
func (w *worker) foundCertCallback(rawEntry *ct.RawLogEntry) {
	entry, parseErr := parseCertstreamEntry(rawEntry, w.operatorName, w.name, w.ctURL)
	if parseErr != nil {
		log.Println("Error parsing certstream entry: ", parseErr)
		return
	}

	entry.Data.UpdateType = "X509LogEntry"
	w.entryChan <- entry

	atomic.AddInt64(&processedCerts, 1)
}

// foundPrecertCallback is the callback that handles cases where new precerts are found.
func (w *worker) foundPrecertCallback(rawEntry *ct.RawLogEntry) {
	entry, parseErr := parseCertstreamEntry(rawEntry, w.operatorName, w.name, w.ctURL)
	if parseErr != nil {
		log.Println("Error parsing certstream entry: ", parseErr)
		return
	}

	entry.Data.UpdateType = "PrecertLogEntry"
	w.entryChan <- entry

	atomic.AddInt64(&processedPrecerts, 1)
}

// certHandler takes the entries out of the entryChan channel and broadcasts them to all clients.
// Only a single instance of the certHandler runs per certstream server.
func certHandler(entryChan chan certstream.Entry) {
	var processed int64

	for {
		entry := <-entryChan
		processed++

		if processed%1000 == 0 {
			log.Printf("Processed %d entries | Queue length: %d\n", processed, len(entryChan))
			// Every thousandth entry, we store one certificate as example
			web.SetExampleCert(entry)
		}

		// Run json encoding in the background and send the result to the clients.
		web.ClientHandler.Broadcast <- entry

		// Update metrics
		url := entry.Data.Source.NormalizedURL
		operator := entry.Data.Source.Operator

		metrics.Inc(operator, url)
	}
}

// getAllLogs returns a list of all CT logs.
func getAllLogs() (loglist3.LogList, error) {
	// Download the list of all logs from ctLogInfo and decode json
	resp, err := http.Get(loglist3.LogListURL)
	if err != nil {
		return loglist3.LogList{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return loglist3.LogList{}, errors.New("failed to download loglist")
	}

	bodyBytes, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		log.Panic(readErr)
	}

	allLogs, parseErr := loglist3.NewFromJSON(bodyBytes)
	if parseErr != nil {
		return loglist3.LogList{}, parseErr
	}

	// Add new ct logs to metrics
	for _, operator := range allLogs.Operators {
		for _, ctlog := range operator.Logs {
			url := normalizeCtlogURL(ctlog.URL)
			metrics.Init(operator.Name, url)
		}
	}

	return *allLogs, nil
}

func normalizeCtlogURL(input string) string {
	input = strings.TrimPrefix(input, "https://")
	input = strings.TrimPrefix(input, "http://")
	input = strings.TrimSuffix(input, "/")

	return input
}

func DownloadAndParseCSV(url string, keyColIndex, valueColIndex int, skipHeader bool) (map[string]string, error) {
	// Initialize result map
	result := make(map[string]string)

	// Maximum number of retry attempts
	maxRetries := 3
	// Initial delay between retries (will be increased exponentially)
	retryDelay := 1 * time.Second

	var resp *http.Response
	var err error

	// Retry logic for the HTTP request
	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Create HTTP client with timeout
		client := &http.Client{
			Timeout: 30 * time.Second,
		}

		// Make the request
		resp, err = client.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			break // Success, exit the retry loop
		}

		// Check if we should retry
		if attempt == maxRetries {
			if err != nil {
				return nil, fmt.Errorf("failed to download CSV after %d attempts: %w", maxRetries, err)
			}
			return nil, fmt.Errorf("failed to download CSV after %d attempts: status code %d", maxRetries, resp.StatusCode)
		}

		// If we got a response but it wasn't successful, close the body
		if err == nil && resp.Body != nil {
			resp.Body.Close()
		}

		// Wait before retrying with exponential backoff
		time.Sleep(retryDelay)
		retryDelay *= 2 // Exponential backoff
	}

	// Don't forget to close the response body when we're done
	defer resp.Body.Close()

	// Parse the CSV data
	reader := csv.NewReader(resp.Body)

	// Read the first row to check column indices and handle header
	firstRow, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("failed to read CSV first row: %w", err)
	}

	// Validate column indices
	if keyColIndex < 0 || keyColIndex >= len(firstRow) {
		return nil, fmt.Errorf("key column index %d is out of range (0-%d)", keyColIndex, len(firstRow)-1)
	}
	if valueColIndex < 0 || valueColIndex >= len(firstRow) {
		return nil, fmt.Errorf("value column index %d is out of range (0-%d)", valueColIndex, len(firstRow)-1)
	}

	// If not skipping header, add the first row to the result
	if !skipHeader {
		result[firstRow[keyColIndex]] = firstRow[valueColIndex]
	}

	// Read the rest of the CSV and populate the map
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break // End of file
		}
		if err != nil {
			return nil, fmt.Errorf("error reading CSV record: %w", err)
		}

		// Convert decoded bytes to lowercase hex without separators
		decodedBytes, _ := base64.StdEncoding.DecodeString(record[keyColIndex])
		hexKey := hex.EncodeToString(decodedBytes)
		hexKey = strings.ToLower(hexKey)
		// Add the key-value pair to our map
		result[hexKey] = record[valueColIndex]

		//log.Printf("CCADB: AKI b64: %v | AKI decoded: %v | CAOwner: %v\n", record[keyColIndex], hexKey, record[valueColIndex])
	}

	//	Simple summary of the CCADB data
	counter := make(map[string]int)
	for _, caName := range result {
		counter[caName]++
	}
	log.Printf("CCADB: Loaded data. Found %v entries for %v distinct CA owners\n", len(result), len(counter))

	return result, nil
}
