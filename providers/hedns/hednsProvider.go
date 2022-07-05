package hedns

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/StackExchange/dnscontrol/v3/models"
	"github.com/StackExchange/dnscontrol/v3/pkg/diff"
	"github.com/StackExchange/dnscontrol/v3/pkg/txtutil"
	"github.com/StackExchange/dnscontrol/v3/providers"
	"github.com/pquerna/otp/totp"
)

/*
Hurricane Electric DNS provider (dns.he.net)

Info required in `creds.json`:
	- username
	- password

Either of the following settings is required when two factor authentication is enabled:
	- totp      (TOTP code if 2FA is enabled; best specified as an env variable)
	- totp-key  (shared TOTP secret used to generate a valid TOTP code; not recommended since
	             this effectively defeats the purpose of two factor authentication by storing
	             both factors at the same place)

Additionally
	- session-file-path  (Path where a '.hedns-session' file will be created to allow a
                         session to persist between executions)

*/

var features = providers.DocumentationNotes{
	providers.CanAutoDNSSEC:          providers.Cannot(),
	providers.CanGetZones:            providers.Can(),
	providers.CanUseAlias:            providers.Can(),
	providers.CanUseCAA:              providers.Can(),
	providers.CanUseDS:               providers.Cannot(),
	providers.CanUseDSForChildren:    providers.Cannot(),
	providers.CanUseNAPTR:            providers.Can(),
	providers.CanUsePTR:              providers.Can(),
	providers.CanUseSRV:              providers.Can(),
	providers.CanUseSSHFP:            providers.Can(),
	providers.CanUseTLSA:             providers.Cannot(),
	providers.DocCreateDomains:       providers.Can(),
	providers.DocDualHost:            providers.Can(),
	providers.DocOfficiallySupported: providers.Cannot(),
}

func init() {
	fns := providers.DspFuncs{
		Initializer:   newHEDNSProvider,
		RecordAuditor: AuditRecords,
	}
	providers.RegisterDomainServiceProviderType("HEDNS", fns, features)
}

var defaultNameservers = []string{
	"ns1.he.net",
	"ns2.he.net",
	"ns3.he.net",
	"ns4.he.net",
	"ns5.he.net",
}

const (
	apiEndpoint     = "https://dns.he.net/"
	sessionFileName = ".hedns-session"

	errorInvalidCredentials = "Incorrect"
	errorInvalidTotpToken   = "The token supplied is invalid."
	errorTotpTokenRequired  = "You must enter the token generated by your authenticator."
	errorTotpTokenReused    = "This token has already been used.  You may not reuse tokens."
	errorImproperDelegation = "This zone does not appear to be properly delegated to our nameservers."
)

// hednsProvider stores login credentials and represents and API connection
type hednsProvider struct {
	Username        string
	Password        string
	TfaSecret       string
	TfaValue        string
	SessionFilePath string

	httpClient http.Client
}

// Record stores the HEDNS specific zone and record IDs
type Record struct {
	RecordName string
	RecordID   uint64
	ZoneName   string
	ZoneID     uint64
}

func newHEDNSProvider(cfg map[string]string, _ json.RawMessage) (providers.DNSServiceProvider, error) {
	username, password := cfg["username"], cfg["password"]
	totpSecret, totpValue := cfg["totp-key"], cfg["totp"]
	sessionFilePath := cfg["session-file-path"]

	if username == "" {
		return nil, fmt.Errorf("username must be provided")
	}
	if password == "" {
		return nil, fmt.Errorf("password must be provided")
	}
	if totpSecret != "" && totpValue != "" {
		return nil, fmt.Errorf("totp and totp-key must not be specified at the same time")
	}

	// Perform the initial login
	client := &hednsProvider{
		Username:        username,
		Password:        password,
		TfaSecret:       totpSecret,
		TfaValue:        totpValue,
		SessionFilePath: sessionFilePath,
	}

	// Create storage for the cookies
	cookieJar, _ := cookiejar.New(nil)
	client.httpClient = http.Client{Jar: cookieJar}

	err := client.authenticate()
	return client, err
}

// ListZones list all zones on this provider.
func (c *hednsProvider) ListZones() ([]string, error) {
	domainsMap, err := c.listDomains()
	if err != nil {
		return nil, err
	}

	domains := make([]string, 0, len(domainsMap))
	for domain := range domainsMap {
		domains = append(domains, domain)
	}

	// Ensure the order is deterministic
	sort.Strings(domains)

	return domains, err
}

// EnsureDomainExists creates the domain if it does not exist.
func (c *hednsProvider) EnsureDomainExists(domain string) error {
	domains, err := c.ListZones()
	if err != nil {
		return err
	}

	for _, d := range domains {
		if d == domain {
			return nil
		}
	}

	return c.createDomain(domain)
}

// GetNameservers returns the default HEDNS nameservers.
func (c *hednsProvider) GetNameservers(_ string) ([]*models.Nameserver, error) {
	return models.ToNameservers(defaultNameservers)
}

// GetDomainCorrections returns a list of corrections for the  domain.
func (c *hednsProvider) GetDomainCorrections(dc *models.DomainConfig) ([]*models.Correction, error) {
	var corrections []*models.Correction

	err := dc.Punycode()
	if err != nil {
		return nil, err
	}

	records, err := c.GetZoneRecords(dc.Name)
	if err != nil {
		return nil, err
	}

	// Get the SOA record to get the ZoneID, then remove it from the list.
	zoneID := uint64(0)
	var prunedRecords models.Records
	for _, r := range records {
		if r.Type == "SOA" {
			zoneID = r.Original.(Record).ZoneID
		} else {
			prunedRecords = append(prunedRecords, r)
		}
	}

	// Normalize
	models.PostProcessRecords(prunedRecords)
	txtutil.SplitSingleLongTxt(dc.Records) // Autosplit long TXT records

	differ := diff.New(dc)
	_, toCreate, toDelete, toModify, err := differ.IncrementalDiff(prunedRecords)
	if err != nil {
		return nil, err
	}

	for _, del := range toDelete {
		record := del.Existing
		corrections = append(corrections, &models.Correction{
			Msg: del.String(),
			F:   func() error { return c.deleteZoneRecord(record) },
		})
	}

	for _, cre := range toCreate {
		record := cre.Desired
		record.Original = Record{
			ZoneName:   dc.Name,
			ZoneID:     zoneID,
			RecordName: cre.Desired.Name,
		}
		corrections = append(corrections, &models.Correction{
			Msg: cre.String(),
			F:   func() error { return c.editZoneRecord(record, true) },
		})
	}

	for _, mod := range toModify {
		record := mod.Desired
		record.Original = Record{
			ZoneName:   dc.Name,
			ZoneID:     zoneID,
			RecordID:   mod.Existing.Original.(Record).RecordID,
			RecordName: mod.Desired.Name,
		}
		corrections = append(corrections, &models.Correction{
			Msg: mod.String(),
			F:   func() error { return c.editZoneRecord(record, false) },
		})
	}

	return corrections, err
}

// GetZoneRecords returns all the records for the given domain
func (c *hednsProvider) GetZoneRecords(domain string) (models.Records, error) {
	var zoneRecords []*models.RecordConfig

	// Get Domain ID
	domains, err := c.listDomains()
	if err != nil {
		return nil, err
	}

	domainID, domainExists := domains[domain]
	if !domainExists {
		return nil, fmt.Errorf("domain %s does not exist", domain)
	}

	queryURL, _ := url.Parse(apiEndpoint)
	q := queryURL.Query()
	q.Add("hosted_dns_zoneid", strconv.FormatUint(domainID, 10))
	q.Add("menu", "edit_zone")
	q.Add("hosted_dns_editzone", "")
	queryURL.RawQuery = q.Encode()

	response, err := c.httpClient.Get(queryURL.String())
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	// Parse the HTML response
	document, err := goquery.NewDocumentFromReader(response.Body)
	if err != nil {
		return nil, err
	}

	// Check we can find the zone records
	if document.Find("#dns_main_content").Size() == 0 {
		return nil, fmt.Errorf("zone records listing failed")
	}

	// Load all the domain records
	recordSelector := "tr.dns_tr, tr.dns_tr_dynamic, tr.dns_tr_locked"
	document.Find(recordSelector).EachWithBreak(func(index int, element *goquery.Selection) bool {
		parser := elementParser{}

		rc := &models.RecordConfig{
			Type: parser.parseStringAttr(element.Find("td > .rrlabel"), "data"),
			TTL:  parser.parseIntElementUint32(element.Find("td:nth-child(5)")),
			Original: Record{
				ZoneName:   domain,
				ZoneID:     domainID,
				RecordName: parser.parseStringElement(element.Find(".dns_view")),
				RecordID:   parser.parseIntAttr(element, "id"),
			},
		}
		data := parser.parseStringAttr(element.Find("td:nth-child(7)"), "data")
		if err != nil {
			return false
		}

		priority := parser.parseIntElementUint16(element.Find("td:nth-child(6)"))
		if parser.err != nil {
			err = parser.err
			return false
		}

		// Ignore record types that dnscontrol does not support
		if rc.Type == "HINFO" || rc.Type == "AFSDB" || rc.Type == "RP" || rc.Type == "LOC" {
			return true
		}

		rc.SetLabelFromFQDN(rc.Original.(Record).RecordName, domain)

		switch rc.Type {
		case "ALIAS":
			err = rc.SetTarget(data)
		case "MX":
			// dns.he.net omits the trailing "." on the hostnames for MX records
			err = rc.SetTargetMX(uint16(priority), data+".")
		case "SRV":
			err = rc.SetTargetSRVPriorityString(uint16(priority), data)
		case "SPF", "TXT":
			rc.Type = "TXT" // Convert to TXT record as SPF is deprecated
			err = rc.SetTargetTXTQuotedFields(data)
		default:
			err = rc.PopulateFromString(rc.Type, data, domain)
		}

		if err != nil {
			return false
		}

		zoneRecords = append(zoneRecords, rc)
		return true
	})

	return zoneRecords, err
}

func (c *hednsProvider) authResumeSession() (authenticated bool, requiresTfa bool, err error) {
	response, err := c.httpClient.Get(apiEndpoint)
	if err != nil {
		return false, false, err
	}
	defer response.Body.Close()

	document, err := c.parseResponseForDocumentAndErrors(response)
	if err != nil {
		// Deal with the edge case where we have attempted to use the same authentication token more than two times
		if err.Error() == errorTotpTokenRequired {
			return false, true, nil
		}
		return false, false, err
	}

	// Look for the presence of the login button or the TFA input
	authenticated = document.Find("#_tlogout").Size() > 0
	requiresTfa = document.Find("input#tfacode").Size() > 0

	return authenticated, requiresTfa, err
}

func (c *hednsProvider) authUsernameAndPassword() (authenticated bool, requiresTfa bool, err error) {
	// Login with username and password
	response, err := c.httpClient.PostForm(apiEndpoint, url.Values{
		"email":  {c.Username},
		"pass":   {c.Password},
		"submit": {"Login!"},
	})
	if err != nil {
		return false, false, err
	}
	defer response.Body.Close()

	document, err := c.parseResponseForDocumentAndErrors(response)
	if err != nil {
		if err.Error() == errorInvalidCredentials {
			err = fmt.Errorf("authentication failed with incorrect username or password")
		}
		if err.Error() == errorTotpTokenRequired {
			return false, true, nil
		}
		return false, false, err
	}

	authenticated = document.Find("#_tlogout").Size() > 0
	requiresTfa = document.Find("input#tfacode").Size() > 0

	// Completed and 2FA is not required
	return authenticated, requiresTfa, err
}

func (c *hednsProvider) auth2FA() (authenticated bool, err error) {

	if c.TfaValue == "" && c.TfaSecret == "" {
		return false, fmt.Errorf("account requires two-factor authentication but neither totp or totp-key were provided")
	}

	if c.TfaValue == "" && c.TfaSecret != "" {
		var err error
		c.TfaValue, err = totp.GenerateCode(c.TfaSecret, time.Now())
		if err != nil {
			return false, err
		}
	}

	response, err := c.httpClient.PostForm(apiEndpoint, url.Values{
		"tfacode": {c.TfaValue},
		"submit":  {"Submit"},
	})
	if err != nil {
		return false, err
	}
	defer response.Body.Close()

	document, err := c.parseResponseForDocumentAndErrors(response)
	if err != nil {
		switch err.Error() {
		case errorInvalidTotpToken:
			err = fmt.Errorf("invalid TOTP token value")
		case errorTotpTokenReused:
			err = fmt.Errorf("TOTP token was reused within its period (30 seconds)")
		}
		return false, err
	}
	authenticated = document.Find("#_tlogout").Size() > 0

	return authenticated, err
}

func (c *hednsProvider) authenticate() error {

	if c.SessionFilePath != "" {
		_ = c.loadSessionFile()
	}

	authenticated, requiresTfa, err := c.authResumeSession()
	if err != nil {
		return err
	}

	if !authenticated {
		// Only perform username and password login if two-factor authentication is not required at this stage
		if !requiresTfa {
			authenticated, requiresTfa, err = c.authUsernameAndPassword()
			if err != nil {
				return err
			}
		}

		// Only perform two-factor authentication if required
		if requiresTfa {
			authenticated, err = c.auth2FA()
			if err != nil {
				return err
			}
		}
	}

	if !authenticated {
		err = fmt.Errorf("unknown authentication failure")
	} else {
		if c.SessionFilePath != "" {
			err = c.saveSessionFile()
		}
	}

	return err
}

func (c *hednsProvider) listDomains() (map[string]uint64, error) {
	response, err := c.httpClient.Get(apiEndpoint)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	document, err := goquery.NewDocumentFromReader(response.Body)
	if err != nil {
		return nil, err
	}

	// Check there are any domains in this account
	domains := make(map[string]uint64)
	if document.Find("#domains_table").Size() == 0 {
		return domains, nil
	}

	// Find all the forward & reverse domains
	recordsSelector := strings.Join([]string{
		"#domains_table > tbody > tr > td:last-child > img",                // Forward records
		"#tabs-advanced .generic_table > tbody > tr > td:last-child > img", // Reverse records
	}, ", ")

	document.Find(recordsSelector).EachWithBreak(func(index int, element *goquery.Selection) bool {
		domainID, idExists := element.Attr("value")
		domainName, nameExists := element.Attr("name")
		if idExists && nameExists {
			domains[domainName], err = strconv.ParseUint(domainID, 10, 64)
			return err == nil
		}
		return true
	})

	return domains, err
}

func (c *hednsProvider) createDomain(domain string) error {
	values := url.Values{
		"action":     {"add_zone"},
		"retmain":    {"0"},
		"add_domain": {domain},
		"submit":     {"Add Domain!"},
	}

	response, err := c.httpClient.PostForm(apiEndpoint, values)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	_, err = c.parseResponseForDocumentAndErrors(response)
	return err
}

func (c *hednsProvider) editZoneRecord(rc *models.RecordConfig, create bool) error {
	values := url.Values{
		"account":             {},
		"menu":                {"edit_zone"},
		"hosted_dns_zoneid":   {strconv.FormatUint(rc.Original.(Record).ZoneID, 10)},
		"hosted_dns_editzone": {"1"},
		"TTL":                 {strconv.FormatUint(uint64(rc.TTL), 10)},
		"Name":                {rc.Name},
	}

	// Select the correct mode and deal with the quirks
	if create {
		values.Set("Type", rc.Type)
		values.Set("hosted_dns_editrecord", "Submit")
		values.Set("hosted_dns_recordid", "")
	} else {
		values.Set("Type", strings.ToLower(rc.Type)) // Lowercase on update
		values.Set("hosted_dns_editrecord", "Update")
		values.Set("hosted_dns_recordid", strconv.FormatUint(rc.Original.(Record).RecordID, 10))
	}

	// Handle priorities
	if create {
		values.Set("Priority", "")
	} else {
		values.Set("Priority", "-")
	}

	// Work out the content
	switch rc.Type {
	case "MX":
		values.Set("Priority", strconv.FormatUint(uint64(rc.MxPreference), 10))
		values.Set("Content", rc.GetTargetField())
	case "SRV":
		values.Del("Content")
		values.Set("Target", rc.GetTargetField())
		values.Set("Priority", strconv.FormatUint(uint64(rc.SrvPriority), 10))
		values.Set("Weight", strconv.FormatUint(uint64(rc.SrvWeight), 10))
		values.Set("Port", strconv.FormatUint(uint64(rc.SrvPort), 10))
	default:
		values.Set("Content", rc.GetTargetCombined())
	}

	response, err := c.httpClient.PostForm(apiEndpoint, values)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	_, err = c.parseResponseForDocumentAndErrors(response)
	return err
}

func (c *hednsProvider) deleteZoneRecord(rc *models.RecordConfig) error {
	values := url.Values{
		"menu":                  {"edit_zone"},
		"hosted_dns_zoneid":     {strconv.FormatUint(rc.Original.(Record).ZoneID, 10)},
		"hosted_dns_recordid":   {strconv.FormatUint(rc.Original.(Record).RecordID, 10)},
		"hosted_dns_editzone":   {"1"},
		"hosted_dns_delrecord":  {"1"},
		"hosted_dns_delconfirm": {"delete"},
	}

	response, err := c.httpClient.PostForm(apiEndpoint, values)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	_, err = c.parseResponseForDocumentAndErrors(response)
	return err
}

func (c *hednsProvider) generateCredentialHash() string {
	hash := sha1.New()
	hash.Write([]byte(c.Username))
	hash.Write([]byte(c.Password))
	hash.Write([]byte(c.TfaSecret))
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func (c *hednsProvider) saveSessionFile() error {
	cookieDomain, err := url.Parse(apiEndpoint)
	if err != nil {
		return err
	}

	// Put the credential hash on the first lines
	entries := []string{
		c.generateCredentialHash(),
	}

	for _, cookie := range c.httpClient.Jar.Cookies(cookieDomain) {
		entries = append(entries, strings.Join([]string{cookie.Name, cookie.Value}, "="))
	}

	fileName := path.Join(c.SessionFilePath, sessionFileName)
	err = ioutil.WriteFile(fileName, []byte(strings.Join(entries, "\n")), 0600)
	return err
}

func (c *hednsProvider) loadSessionFile() error {
	cookieDomain, err := url.Parse(apiEndpoint)
	if err != nil {
		return err
	}

	fileName := path.Join(c.SessionFilePath, sessionFileName)
	bytes, err := ioutil.ReadFile(fileName)
	if err != nil {
		if os.IsNotExist(err) {
			// Skip loading the session.
			return nil
		}
		return err
	}

	var cookies []*http.Cookie
	for i, entry := range strings.Split(string(bytes), "\n") {
		if i == 0 {
			if entry != c.generateCredentialHash() {
				return fmt.Errorf("invalid credential hash in session file")
			}
		} else {
			kv := strings.Split(entry, "=")
			if len(kv) == 2 {
				cookies = append(cookies, &http.Cookie{
					Name:  kv[0],
					Value: kv[1],
				})
			}
		}
	}
	c.httpClient.Jar.SetCookies(cookieDomain, cookies)

	return err
}

func (c *hednsProvider) parseResponseForDocumentAndErrors(response *http.Response) (document *goquery.Document, err error) {
	var ignoredErrorMessages = [...]string{
		errorImproperDelegation,
	}

	document, err = goquery.NewDocumentFromReader(response.Body)
	if err != nil {
		return nil, err
	}

	// Check for any errors ignoring irrelevant errors
	document.Find("div#dns_err").EachWithBreak(func(index int, element *goquery.Selection) bool {
		errorMessage := element.Text()
		for _, ignoredMessage := range ignoredErrorMessages {
			if strings.Contains(errorMessage, ignoredMessage) {
				return true
			}
		}
		err = fmt.Errorf(element.Text())
		return false
	})

	return document, err
}

type elementParser struct {
	err error
}

func (p *elementParser) parseStringAttr(element *goquery.Selection, attr string) (result string) {
	if p.err != nil {
		return
	}
	result, exists := element.Attr(attr)
	if !exists {
		p.err = fmt.Errorf("could not locate attribute %s", attr)
	}
	return result
}

func (p *elementParser) parseIntAttr(element *goquery.Selection, attr string) (result uint64) {
	if p.err != nil {
		return
	}
	if value, exists := element.Attr(attr); exists {
		result, p.err = strconv.ParseUint(value, 10, 64)
	} else {
		p.err = fmt.Errorf("could not locate attribute %s", attr)
	}
	return result
}

func (p *elementParser) parseStringElement(element *goquery.Selection) (result string) {
	if p.err != nil {
		return
	}
	return element.Text()
}

func (p *elementParser) parseIntElementUint16(element *goquery.Selection) uint16 {
	if p.err != nil {
		return 0
	}

	// Special case to deal with Priority
	if element.Text() == "-" {
		return 0
	}

	var result64 uint64
	result64, p.err = strconv.ParseUint(element.Text(), 10, 16)
	return uint16(result64)
}

func (p *elementParser) parseIntElementUint32(element *goquery.Selection) uint32 {
	if p.err != nil {
		return 0
	}

	// Special case to deal with Priority
	if element.Text() == "-" {
		return 0
	}

	var result64 uint64
	result64, p.err = strconv.ParseUint(element.Text(), 10, 32)
	return uint32(result64)
}
