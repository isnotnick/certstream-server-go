package certificatetransparency

import (
	"bytes"
	"crypto/dsa"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"log"
	"math/big"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/d-Rickyy-b/certstream-server-go/internal/certstream"

	psl "golang.org/x/net/publicsuffix"

	ct "github.com/google/certificate-transparency-go"
	"github.com/google/certificate-transparency-go/x509"
	"github.com/google/certificate-transparency-go/x509/pkix"
)

// JSON version of pkix.Name
type JSONName struct {
	CommonName         string        `json:"common_name,omitempty"`
	SerialNumber       string        `json:"serial_number,omitempty"`
	Country            string        `json:"country,omitempty"`
	Organization       string        `json:"organization,omitempty"`
	OrganizationalUnit string        `json:"organizational_unit,omitempty"`
	Locality           string        `json:"locality,omitempty"`
	Province           string        `json:"province,omitempty"`
	StreetAddress      string        `json:"street_address,omitempty"`
	PostalCode         string        `json:"postal_code,omitempty"`
	Names              []interface{} `json:"names,omitempty"`
}

// parseData converts a *ct.RawLogEntry struct into a certstream.Data struct by copying some values and calculating others.
func parseData(entry *ct.RawLogEntry, operatorName, logName, ctURL string) (certstream.Data, error) {
	certLink := fmt.Sprintf("%s/ct/v1/get-entries?start=%d&end=%d", ctURL, entry.Index, entry.Index)

	// Create main data structure
	data := certstream.Data{
		CertIndex: entry.Index,
		CertLink:  certLink,
		Seen:      float64(time.Now().UnixMilli()) / 1_000,
		Source: certstream.Source{
			Name:          logName,
			URL:           ctURL,
			Operator:      operatorName,
			NormalizedURL: normalizeCtlogURL(ctURL),
		},
		UpdateType: "X509LogEntry",
	}

	// Convert RawLogEntry to ct.LogEntry
	logEntry, conversionErr := entry.ToLogEntry()
	if conversionErr != nil {
		log.Println("Could not convert entry to LogEntry: ", conversionErr)
		return certstream.Data{}, conversionErr
	}

	var cert *x509.Certificate
	var rawData []byte
	var isPrecert bool

	switch {
	case logEntry.X509Cert != nil:
		cert = logEntry.X509Cert
		rawData = logEntry.X509Cert.Raw
		isPrecert = false
	case logEntry.Precert != nil:
		cert = logEntry.Precert.TBSCertificate
		rawData = logEntry.Precert.Submitted.Data
		isPrecert = true
	default:
		return certstream.Data{}, errors.New("could not parse entry: no certificate found")
	}

	// Calculate certificate hash from the raw DER bytes of the certificate
	data.LeafCert = leafCertFromX509cert(*cert)

	// recalculate hashes if the certificate is a precertificate
	if isPrecert {
		calculatedHash := calculateSHA1(rawData)
		data.LeafCert.Fingerprint = calculatedHash
		data.LeafCert.SHA1 = calculatedHash
		data.LeafCert.SHA256 = calculateSHA256(rawData)
	}

	certAsDER := base64.StdEncoding.EncodeToString(entry.Cert.Data)
	data.LeafCert.AsDER = certAsDER

	var parseErr error
	data.Chain, parseErr = parseCertificateChain(logEntry)
	if parseErr != nil {
		log.Println("Could not parse certificate chain: ", parseErr)
		return certstream.Data{}, parseErr
	}

	return data, nil
}

// parseCertificateChain returns the certificate chain in form of a []LeafCert from the given *ct.LogEntry.
func parseCertificateChain(logEntry *ct.LogEntry) ([]certstream.LeafCert, error) {
	chain := make([]certstream.LeafCert, len(logEntry.Chain))

	for i, chainEntry := range logEntry.Chain {
		myCert, parseErr := x509.ParseCertificate(chainEntry.Data)
		if parseErr != nil {
			log.Println("Error parsing certificate: ", parseErr)
			return nil, parseErr
		}

		leafCert := leafCertFromX509cert(*myCert)
		chain[i] = leafCert
	}

	return chain, nil
}

// Parse Go's pkix.Name into a JSON
func ParseNameJSON(name pkix.Name) JSONName {
	n := JSONName{
		CommonName:         name.CommonName,
		SerialNumber:       name.SerialNumber,
		Country:            strings.Join(name.Country, ","),
		Organization:       strings.Join(name.Organization, ","),
		OrganizationalUnit: strings.Join(name.OrganizationalUnit, ","),
		Locality:           strings.Join(name.Locality, ","),
		Province:           strings.Join(name.Province, ","),
		StreetAddress:      strings.Join(name.StreetAddress, ","),
		PostalCode:         strings.Join(name.PostalCode, ","),
	}

	for i := range name.Names {
		n.Names = append(n.Names, name.Names[i].Value)
	}

	return n
}

// leafCertFromX509cert converts a x509.Certificate to the custom LeafCert data structure.
func leafCertFromX509cert(cert x509.Certificate) certstream.LeafCert {
	leafCert := certstream.LeafCert{
		AllDomains:         cert.DNSNames,
		Extensions:         certstream.Extensions{},
		NotAfter:           cert.NotAfter.Unix(),
		NotBefore:          cert.NotBefore.Unix(),
		SerialNumber:       formatSerialNumber(cert.SerialNumber),
		SignatureAlgorithm: parseSignatureAlgorithm(cert.SignatureAlgorithm),
		KeyType:            parseKeyType(cert.PublicKeyAlgorithm, cert.RawSubjectPublicKeyInfo),
		IsCA:               cert.IsCA,
	}

	// The zero value of DomainsEntry.Data is nil, but we want an empty array - especially for json marshalling later.
	if leafCert.AllDomains == nil {
		leafCert.AllDomains = []string{}
	}

	leafCert.Subject = buildSubject(cert.Subject)
	wildcardCount := 0
	regDomainSlice := []string{}
	if *leafCert.Subject.CN != "" && !leafCert.IsCA {
		domainAlreadyAdded := false
		// TODO check if CN matches domain regex
		for _, domain := range leafCert.AllDomains {
			//	Check for wildcards
			if strings.Contains(domain, "*") {
				wildcardCount++
			}
			//	Extract 'registerable domain' or 'effective domain plus one' from each SAN
			isIP := net.ParseIP(domain)
			if isIP == nil {
				regDomain, err := psl.EffectiveTLDPlusOne(domain)
				if err != nil {
					regDomainSlice = append(regDomainSlice, domain)
				} else {
					regDomainSlice = append(regDomainSlice, regDomain)
				}
			} else {
				regDomainSlice = append(regDomainSlice, domain)
			}

			if domain == *leafCert.Subject.CN {
				domainAlreadyAdded = true
				//break
			}
		}

		if !domainAlreadyAdded {
			leafCert.AllDomains = append(leafCert.AllDomains, *leafCert.Subject.CN)
		}
	}

	leafCert.Issuer = buildSubject(cert.Issuer)

	leafCert.AsDER = base64.StdEncoding.EncodeToString(cert.Raw)
	leafCert.Fingerprint = calculateSHA1(cert.Raw)
	leafCert.SHA1 = leafCert.Fingerprint
	leafCert.SHA256 = calculateSHA256(cert.Raw)

	// TODO fix Extensions - check x509util.go
	for _, extension := range cert.Extensions {
		switch {
		case extension.Id.Equal(x509.OIDExtensionAuthorityKeyId):
			leafCert.Extensions.AuthorityKeyIdentifier = formatKeyIDShort(cert.AuthorityKeyId)
		case extension.Id.Equal(x509.OIDExtensionKeyUsage):
			keyUsage := keyUsageToString(cert.KeyUsage)
			leafCert.Extensions.KeyUsage = &keyUsage
		case extension.Id.Equal(x509.OIDExtensionSubjectKeyId):
			leafCert.Extensions.SubjectKeyIdentifier = formatKeyID(cert.SubjectKeyId)
		case extension.Id.Equal(x509.OIDExtensionBasicConstraints):
			isCA := strings.ToUpper(fmt.Sprintf("CA:%t", cert.IsCA))
			leafCert.Extensions.BasicConstraints = &isCA
		case extension.Id.Equal(x509.OIDExtensionSubjectAltName):
			var buf bytes.Buffer
			for _, name := range cert.DNSNames {
				commaAppend(&buf, "DNS:"+name)
			}

			for _, email := range cert.EmailAddresses {
				commaAppend(&buf, "email:"+email)
			}

			for _, ip := range cert.IPAddresses {
				commaAppend(&buf, "IP Address:"+ip.String())
			}

			subjectAltName := buf.String()
			leafCert.Extensions.SubjectAltName = &subjectAltName
		case extension.Id.Equal(x509.OIDExtensionAuthorityInfoAccess):
			var buf bytes.Buffer
			for _, issuer := range cert.IssuingCertificateURL {
				commaAppend(&buf, "URI:"+issuer)
			}

			for _, ocsp := range cert.OCSPServer {
				commaAppend(&buf, "URI:"+ocsp)
			}

			result := buf.String()
			leafCert.Extensions.AuthorityInfoAccess = &result
		case extension.Id.Equal(x509.OIDExtensionCTPoison):
			leafCert.Extensions.CTLPoisonByte = true
		}
	}

	//	Certificate validation type determination
	//	Try some of the policy OIDs that some CAs add
	leafCert.ValidationType = "OV"
	PolicyOIDSString := fmt.Sprintf("%d", cert.PolicyIdentifiers)
	if strings.Contains(PolicyOIDSString, "2.23.140.1.2.1") {
		leafCert.ValidationType = "DV"
	} else if strings.Contains(PolicyOIDSString, "2.23.140.1.2.2") {
		leafCert.ValidationType = "OV"
	} else if strings.Contains(PolicyOIDSString, "2.23.140.1.2.3") {
		leafCert.ValidationType = "IV"
	} else if strings.Contains(PolicyOIDSString, "2.23.140.1.1") {
		leafCert.ValidationType = "EV"
	}
	//	Now some basic checks
	//	No Subject O - it's a DV
	if leafCert.Subject.O == nil {
		leafCert.ValidationType = "DV"
	}

	//	There's a 'jurisdictionC' in the Subject, so it's an EV
	if strings.Contains(*leafCert.Subject.Aggregated, "1.3.6.1.4.1.311.60.2.1.3") {
		leafCert.ValidationType = "EV"
	}

	//	Certificate 'type' determination and SAN/domain information - already checked for wildcards above
	if wildcardCount > 0 {
		leafCert.CertType = "Wildcard"
	} else if len(leafCert.AllDomains) > 2 {
		leafCert.CertType = "Multi"
	} else {
		leafCert.CertType = "Single"
	}

	//	cert_type_ext is san count and number of single/wildcards
	//	TODO: Detect and split iPAddresses in the SAN
	leafCert.CertTypeExt.SANCount = len(leafCert.AllDomains)
	leafCert.CertTypeExt.WildcardSANCount = wildcardCount
	leafCert.CertTypeExt.SingleSANCount = leafCert.CertTypeExt.SANCount - leafCert.CertTypeExt.WildcardSANCount

	// De-duplicate the reg-domain slice
	seenRegDomain := map[string]bool{}
	var regDomainResult []string
	for v := range regDomainSlice {
		if !seenRegDomain[regDomainSlice[v]] {
			seenRegDomain[regDomainSlice[v]] = true
			regDomainResult = append(regDomainResult, regDomainSlice[v])
		}
	}
	leafCert.AllRegDomains = regDomainResult

	//	CA owner from the periodically-updated Owner map
	leafAKI := *formatKeyIDShort(cert.AuthorityKeyId)
	caOwnerCheck, ok := CAOwners[leafAKI]
	if ok {
		leafCert.CAOwner = caOwnerCheck
	} else {
		leafCert.CAOwner = "unknown"
	}

	return leafCert
}

// buildSubject generates a Subject struct from the given pkix.Name.
func buildSubject(certSubject pkix.Name) certstream.Subject {
	subject := certstream.Subject{
		C:  parseName(certSubject.Country),
		CN: &certSubject.CommonName,
		L:  parseName(certSubject.Locality),
		O:  parseName(certSubject.Organization),
		OU: parseName(certSubject.OrganizationalUnit),
		ST: parseName(certSubject.StreetAddress),
	}
	/*
		if subject.C != nil {
			aggregated += fmt.Sprintf("/C=%s", *subject.C)
		}

		if subject.CN != nil {
			aggregated += fmt.Sprintf("/CN=%s", *subject.CN)
		}

		if subject.L != nil {
			aggregated += fmt.Sprintf("/L=%s", *subject.L)
		}

		if subject.O != nil {
			aggregated += fmt.Sprintf("/O=%s", *subject.O)
		}

		if subject.OU != nil {
			aggregated += fmt.Sprintf("/OU=%s", *subject.OU)
		}

		if subject.ST != nil {
			aggregated += fmt.Sprintf("/ST=%s", *subject.ST)
		}
	*/
	aggregatedJSON, _ := json.Marshal(ParseNameJSON(certSubject))
	jsonSubject := string(aggregatedJSON)
	subject.Aggregated = &jsonSubject

	return subject
}

// formatKeyID transforms the AuthorityKeyIdentifier to be more readable.
func formatKeyID(keyID []byte) *string {
	tmp := hex.EncodeToString(keyID)
	var digest string

	for i := 0; i < len(tmp); i += 2 {
		digest = digest + ":" + tmp[i:i+2]
	}

	digest = strings.TrimLeft(digest, ":")
	digest = fmt.Sprintf("keyid:%s", digest)

	return &digest
}

// formatKeyIDShort transforms the AuthorityKeyIdentifier to be more readable.
func formatKeyIDShort(keyID []byte) *string {
	tmp := hex.EncodeToString(keyID)
	digest := strings.ToLower(tmp)

	return &digest
}

func formatSerialNumber(serialNumber *big.Int) string {
	sn := fmt.Sprintf("%X", serialNumber)
	if len(sn)%2 == 1 {
		sn = "0" + sn
	}

	return sn
}

func parseName(input []string) *string {
	if input == nil {
		return nil
	}

	var result string
	for _, s := range input {
		if len(result) > 0 {
			result += ","
		}

		result += s
	}

	return &result
}

// calculateHash takes a hash.Hash struct and calculates the fingerprint of the given data.
func calculateHash(data []byte, certHasher hash.Hash) string {
	_, e := certHasher.Write(data)
	if e != nil {
		log.Printf("Error while hashing cert: %s\n", e)
		return ""
	}

	certHash := fmt.Sprintf("%02x", certHasher.Sum(nil))
	certHash = strings.ToUpper(certHash)

	var result bytes.Buffer
	for i := 0; i < len(certHash); i++ {
		if i%2 == 0 && i > 0 {
			result.WriteByte(':')
		}
		c := certHash[i]
		result.WriteByte(c)
	}

	return result.String()
}

// calculateSHA1 calculates the SHA1 fingerprint of the given data.
func calculateSHA1(data []byte) string {
	return calculateHash(data, sha1.New()) //nolint:gosec
}

// calculateSHA256 calculates the SHA256 fingerprint of the given data.
func calculateSHA256(data []byte) string {
	return calculateHash(data, sha256.New())
}

// Calculate key type and size
func parseKeyType(keyAlg x509.PublicKeyAlgorithm, rawKey []byte) string {
	switch keyAlg {
	case 0:
		return "Unknown"
	case 1:
		rsaKey, err := x509.ParsePKIXPublicKey(rawKey)
		if err == nil {
			rsaPub := rsaKey.(*rsa.PublicKey)
			KeySize := rsaPub.N
			keySizeBits := strconv.Itoa(KeySize.BitLen())
			return "RSA" + keySizeBits
		}
	case 2:
		dsaKey, err := x509.ParsePKIXPublicKey(rawKey)
		if err == nil {
			dsaPub := dsaKey.(*dsa.PublicKey)
			KeySize := dsaPub.Y
			keySizeBits := strconv.Itoa(KeySize.BitLen())
			return "DSA" + keySizeBits
		}
	case 3:
		ecdsaKey, err := x509.ParsePKIXPublicKey(rawKey)
		if err == nil {
			ecdsaPub := ecdsaKey.(*ecdsa.PublicKey)
			KeySize := ecdsaPub.X
			keySizeBits := strconv.Itoa(KeySize.BitLen())
			return "ECDSA" + keySizeBits
		}
	default:
		return "Unknown"
	}
	return "Unknown"
}

func parseSignatureAlgorithm(signatureAlgoritm x509.SignatureAlgorithm) string {
	switch signatureAlgoritm {
	case x509.MD2WithRSA:
		return "MD2WithRSA"
	case x509.MD5WithRSA:
		return "MD5WithRSA"
	case x509.SHA1WithRSA:
		return "SHA1WithRSA"
	case x509.SHA256WithRSA:
		return "SHA256WithRSA"
	case x509.SHA384WithRSA:
		return "SHA384WithRSA"
	case x509.SHA512WithRSA:
		return "SHA512WithRSA"
	case x509.SHA256WithRSAPSS:
		return "SHA256WithRSAPSS"
	case x509.SHA384WithRSAPSS:
		return "SHA384WithRSAPSS"
	case x509.SHA512WithRSAPSS:
		return "SHA512WithRSAPSS"
	case x509.DSAWithSHA1:
		return "DSAWithSHA1"
	case x509.DSAWithSHA256:
		return "DSAWithSHA256"
	case x509.ECDSAWithSHA1:
		return "ECDSAWithSHA1"
	case x509.ECDSAWithSHA256:
		return "ECDSAWithSHA256"
	case x509.ECDSAWithSHA384:
		return "ECDSAWithSHA384"
	case x509.ECDSAWithSHA512:
		return "ECDSAWithSHA512"
	case x509.PureEd25519:
		return "PureEd25519"
	case x509.UnknownSignatureAlgorithm:
		fallthrough
	default:
		return "unknown"
	}
}

// commaAppend lets you append a string with a comma prepended to a buffer.
func commaAppend(buf *bytes.Buffer, s string) {
	if buf.Len() > 0 {
		buf.WriteString(", ")
	}

	buf.WriteString(s)
}

func keyUsageToString(k x509.KeyUsage) string {
	var buf bytes.Buffer
	if k&x509.KeyUsageDigitalSignature != 0 {
		commaAppend(&buf, "Digital Signature")
	}

	if k&x509.KeyUsageContentCommitment != 0 {
		commaAppend(&buf, "Content Commitment")
	}

	if k&x509.KeyUsageKeyEncipherment != 0 {
		commaAppend(&buf, "Key Encipherment")
	}

	if k&x509.KeyUsageDataEncipherment != 0 {
		commaAppend(&buf, "Data Encipherment")
	}

	if k&x509.KeyUsageKeyAgreement != 0 {
		commaAppend(&buf, "Key Agreement")
	}

	if k&x509.KeyUsageCertSign != 0 {
		commaAppend(&buf, "Certificate Signing")
	}

	if k&x509.KeyUsageCRLSign != 0 {
		commaAppend(&buf, "CRL Signing")
	}

	if k&x509.KeyUsageEncipherOnly != 0 {
		commaAppend(&buf, "Encipher Only")
	}

	if k&x509.KeyUsageDecipherOnly != 0 {
		commaAppend(&buf, "Decipher Only")
	}

	return buf.String()
}

// parseCertstreamEntry creates an Entry from a ct.RawLogEntry.
func parseCertstreamEntry(rawEntry *ct.RawLogEntry, operatorName, logname, ctURL string) (certstream.Entry, error) {
	if rawEntry == nil {
		return certstream.Entry{}, errors.New("certstream entry is nil")
	}

	data, err := parseData(rawEntry, operatorName, logname, ctURL)
	if err != nil {
		return certstream.Entry{}, err
	}

	entry := certstream.Entry{
		Data:        data,
		MessageType: "certificate_update",
	}

	return entry, nil
}
