// Package acmeapi provides an API for accessing ACME servers.
//
// Some methods provided correspond exactly to ACME calls, such as
// NewAuthorization, RespondToChallenge, RequestCertificate or Revoke. Others,
// such as UpsertRegistration, LoadCertificate or WaitForCertificate,
// automatically compose requests to provide a simplified interface.
//
// For example, LoadCertificate obtains the issuing certificate chain as well.
// WaitForCertificate polls until a certificate is available.
// UpsertRegistration determines automatically whether an account key is
// already registered and registers it if it is not.
//
// All methods take Contexts so as to support cancellation and timeouts.
//
// If you have an URI for an authorization, challenge or certificate, you
// can load it by constructing such an object and setting the URI field,
// then calling the appropriate Load function. (The unexported fields in these
// structures are used to track Retry-After times for the WaitLoad* functions and
// are not a barrier to you constructing these objects.)
//
// The following additional packages are likely to be of interest:
//
//   https://godoc.org/github.com/hlandau/acme/responder    Challenge type implementations
//   https://godoc.org/github.com/hlandau/acme/solver       Authorization solver
//   https://godoc.org/github.com/hlandau/acme/acmeutils    Certificate loading utilities
//
package acmeapi

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"github.com/square/go-jose"

	denet "github.com/hlandau/degoutils/net"
	"github.com/peterhellberg/link"
	"golang.org/x/net/context"
	"golang.org/x/net/context/ctxhttp"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"

	"encoding/json"
	"fmt"
	"github.com/hlandau/xlog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Log site.
var log, Log = xlog.NewQuiet("acme.api")

const (
	// Let's Encrypt Live ACME Directory URL.
	LELiveURL = "https://acme-v01.api.letsencrypt.org/directory"
	// Let's Encrypt Staging ACME Directory URL.
	LEStagingURL = "https://acme-staging.api.letsencrypt.org/directory"
)

// Default provider to use. Currently defaults to the Let's Encrypt staging server.
var DefaultDirectoryURL = LEStagingURL

type directoryInfo struct {
	NewReg     string `json:"new-reg"`
	RecoverReg string `json:"recover-reg"`
	NewAuthz   string `json:"new-authz"`
	NewCert    string `json:"new-cert"`
	RevokeCert string `json:"revoke-cert"`
}

type regInfo struct {
	Resource string `json:"resource"` // must be "new-reg" or "reg"

	Contact []string         `json:"contact,omitempty"`
	Key     *jose.JsonWebKey `json:"key,omitempty"`

	AgreementURI      string `json:"agreement,omitempty"`
	AuthorizationsURI string `json:"authorizations,omitempty"`
	CertificatesURI   string `json:"certificates,omitempty"`
}

type revokeReq struct {
	Resource    string         `json:"resource"` // "revoke-cert"
	Certificate denet.Base64up `json:"certificate"`
}

// Represents an identifier for which an authorization is desired.
type Identifier struct {
	Type  string `json:"type"`  // must be "dns"
	Value string `json:"value"` // dns: a hostname.
}

// Represents the status of an authorization or challenge.
type Status string

const (
	StatusUnknown    Status = "unknown"
	StatusPending           = "pending"
	StatusProcessing        = "processing"
	StatusValid             = "valid"
	StatusInvalid           = "invalid"
	StatusRevoked           = "revoked"
)

// Returns true iff the status is a valid status.
func (s Status) Valid() bool {
	switch s {
	case "unknown", "pending", "processing", "valid", "invalid", "revoked":
		return true
	default:
		return false
	}
}

// Returns true iff the status is a final status.
func (s Status) Final() bool {
	switch s {
	case "valid", "invalid", "revoked":
		return true
	default:
		return false
	}
}

func (s *Status) UnmarshalJSON(data []byte) error {
	var ss string
	err := json.Unmarshal(data, &ss)
	if err != nil {
		return err
	}

	if !Status(ss).Valid() {
		return fmt.Errorf("not a valid status: %#v", ss)
	}

	*s = Status(ss)
	return nil
}

// Represents a Challenge which is part of an Authorization.
type Challenge struct {
	URI      string `json:"uri"`      // The URI of the challenge.
	Resource string `json:"resource"` // "challenge"

	Type      string    `json:"type"`
	Status    Status    `json:"status,omitempty"`
	Validated time.Time `json:"validated,omitempty"` // RFC 3339
	Token     string    `json:"token"`

	// tls-sni-01
	N int `json:"n,omitempty"`

	// proofOfPossession
	Certs []denet.Base64up `json:"certs,omitempty"`

	retryAt time.Time
}

// Represents an authorization. You can construct an authorization from only
// the URI; the authorization information will be fetched automatically.
type Authorization struct {
	URI      string `json:"-"`        // The URI of the authorization.
	Resource string `json:"resource"` // must be "new-authz" or "authz"

	Identifier   Identifier   `json:"identifier"`
	Status       Status       `json:"status,omitempty"`
	Expires      time.Time    `json:"expires,omitempty"` // RFC 3339 (ISO 8601)
	Challenges   []*Challenge `json:"challenges,omitempty"`
	Combinations [][]int      `json:"combinations,omitempty"`

	retryAt time.Time
}

// Represents a certificate which has been, or is about to be, issued.
type Certificate struct {
	URI      string `json:"-"`        // The URI of the certificate.
	Resource string `json:"resource"` // "new-cert"

	// The certificate data. DER.
	Certificate []byte `json:"-"`

	// Any required extra certificates, in DER form in the correct order.
	ExtraCertificates [][]byte `json:"-"`

	// DER. Consumers of this API will find that this is always nil; it is
	// used internally when submitting certificate requests.
	CSR denet.Base64up `json:"csr"`

	retryAt time.Time
}

// Client for making ACME API calls.
//
// You must set at least AccountKey.
type Client struct {
	AccountInfo struct {
		// Account private key. Required.
		AccountKey crypto.PrivateKey

		// Set of agreement URIs to automatically accept.
		AgreementURIs map[string]struct{}

		// Registration URI, if found. You can set this if known, which will save a
		// round trip in some cases. Optional.
		RegistrationURI string

		// Contact URIs. These will be used when registering or when updating a
		// registration. Optional.
		ContactURIs []string
	}

	// The ACME server directory URL. Defaults to DefaultBaseURL.
	DirectoryURL string

	// Uses http.DefaultClient if nil.
	HTTPClient *http.Client

	dir         *directoryInfo
	nonceSource nonceSource
	initOnce    sync.Once
}

// Error returned when the account agreement URI does not match the currently required
// agreement URI.
type AgreementError struct {
	URI string // The required agreement URI.
}

func (e *AgreementError) Error() string {
	return fmt.Sprintf("Registration requires agreement with the following agreement: %#v", e.URI)
}

type httpError struct {
	Res         *http.Response
	ProblemBody string
}

func (he *httpError) Error() string {
	return fmt.Sprintf("HTTP error: %v\n%v\n%v", he.Res.Status, he.Res.Header, he.ProblemBody)
}

func newHTTPError(res *http.Response) error {
	he := &httpError{
		Res: res,
	}
	if res.Header.Get("Content-Type") == "application/problem+json" {
		defer res.Body.Close()
		b, err := ioutil.ReadAll(res.Body)
		if err == nil {
			he.ProblemBody = string(b)
		}
	}
	return he
}

func (c *Client) doReq(method, url string, v, r interface{}, ctx context.Context) (*http.Response, error) {
	return c.doReqEx(method, url, nil, v, r, ctx)
}

func algorithmFromKey(key crypto.PrivateKey) (jose.SignatureAlgorithm, error) {
	switch v := key.(type) {
	case *rsa.PrivateKey:
		return jose.RS256, nil
	case *ecdsa.PrivateKey:
		name := v.Curve.Params().Name
		switch name {
		case "P-256":
			return jose.ES256, nil
		case "P-384":
			return jose.ES384, nil
		case "P-521":
			return jose.ES512, nil
		default:
			return "", fmt.Errorf("unsupported ECDSA curve: %s", name)
		}
	default:
		return "", fmt.Errorf("unsupported private key type: %T", key)
	}
}

func (c *Client) doReqEx(method, url string, key crypto.PrivateKey, v, r interface{}, ctx context.Context) (*http.Response, error) {
	if TestingNoTLS && strings.HasPrefix(url, "https:") {
		url = "http" + url[5:]
	}
	if !ValidURL(url) {
		return nil, fmt.Errorf("invalid URL: %#v", url)
	}

	if key == nil {
		key = c.AccountInfo.AccountKey
	}

	var rdr io.Reader
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}

		if key == nil {
			return nil, fmt.Errorf("account key must be specified")
		}

		kalg, err := algorithmFromKey(key)
		if err != nil {
			return nil, err
		}

		signer, err := jose.NewSigner(kalg, key)
		if err != nil {
			return nil, err
		}

		signer.SetNonceSource(&c.nonceSource)

		sig, err := signer.Sign(b)
		if err != nil {
			return nil, err
		}

		s := sig.FullSerialize()
		if err != nil {
			return nil, err
		}

		rdr = strings.NewReader(s)
	}

	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "acmetool")
	req.Header.Set("Accept", "application/json")
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}

	log.Debugf("request: %s", url)
	res, err := ctxhttp.Do(ctx, c.HTTPClient, req)
	log.Debugf("response: %v %v", res, err)
	if err != nil {
		return nil, err
	}

	if n := res.Header.Get("Replay-Nonce"); n != "" {
		c.nonceSource.AddNonce(n)
	}

	if res.StatusCode >= 400 && res.StatusCode < 600 {
		defer res.Body.Close()
		return res, newHTTPError(res)
	}

	if r != nil {
		defer res.Body.Close()
		if ct := res.Header.Get("Content-Type"); ct != "application/json" {
			return res, fmt.Errorf("unexpected content type: %#v", ct)
		}

		err = json.NewDecoder(res.Body).Decode(r)
		if err != nil {
			return nil, err
		}
	}

	return res, nil
}

// Returns true if the URL given is (potentially) a valid ACME resource URL.
//
// The URL must be an HTTPS URL.
func ValidURL(u string) bool {
	ur, err := url.Parse(u)
	return err == nil && (ur.Scheme == "https" || (TestingNoTLS && ur.Scheme == "http"))
}

// Internal use only. All ACME URLs must use "https" and not "http". However,
// for testing purposes, if this is set, "https" URLs will be retrieved as
// though they are "http" URLs. This is useful for testing when a test ACME
// server doesn't have SSL configured.
var TestingNoTLS = false

func parseRetryAfter(h http.Header) (t time.Time, ok bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return time.Time{}, false
	}

	n, err := strconv.ParseUint(v, 10, 31)
	if err != nil {
		t, err = time.Parse(time.RFC1123, v)
		if err != nil {
			return time.Time{}, false
		}

		return t, true
	}

	return time.Now().Add(time.Duration(n) * time.Second), true
}

func retryAtDefault(h http.Header, d time.Duration) time.Time {
	t, ok := parseRetryAfter(h)
	if ok {
		return t
	}

	return time.Now().Add(d)
}

func (c *Client) getDirectory(ctx context.Context) (*directoryInfo, error) {
	if c.dir != nil {
		return c.dir, nil
	}

	if c.DirectoryURL == "" {
		c.DirectoryURL = DefaultDirectoryURL
	}

	_, err := c.doReq("GET", c.DirectoryURL, nil, &c.dir, ctx)
	if err != nil {
		return nil, err
	}

	if !ValidURL(c.dir.NewReg) || !ValidURL(c.dir.NewAuthz) || !ValidURL(c.dir.NewCert) {
		c.dir = nil
		return nil, fmt.Errorf("directory does not provide required endpoints")
	}

	return c.dir, nil
}

// API Methods

// Find the registration URI, by registering a new account if necessary.
func (c *Client) getRegistrationURI(ctx context.Context) (string, error) {
	if c.AccountInfo.RegistrationURI != "" {
		return c.AccountInfo.RegistrationURI, nil
	}

	di, err := c.getDirectory(ctx)
	if err != nil {
		return "", err
	}

	reqInfo := regInfo{
		Resource: "new-reg",
		Contact:  c.AccountInfo.ContactURIs,
	}

	var resInfo *regInfo
	res, err := c.doReq("POST", di.NewReg, &reqInfo, &resInfo, ctx)
	if res == nil {
		return "", err
	}

	if res.StatusCode == 201 || res.StatusCode == 409 {
		loc := res.Header.Get("Location")
		if !ValidURL(loc) {
			return "", fmt.Errorf("invalid URL: %#v", loc)
		}

		c.AccountInfo.RegistrationURI = loc
	} else if err != nil {
		return "", err
	} else {
		return "", fmt.Errorf("unexpected status code: %v", res.StatusCode)
	}

	return c.AccountInfo.RegistrationURI, nil
}

// Registers a new account or updates an existing account.
//
// The ContactURIs specified will be set.
//
// If a new agreement is required and it is set in AgreementURIs, it will be
// agreed to automatically. Otherwise AgreementError will be returned.
func (c *Client) UpsertRegistration(ctx context.Context) error {
	regURI, err := c.getRegistrationURI(ctx)
	if err != nil {
		return err
	}

	reqInfo := regInfo{
		Resource: "reg",
		Contact:  c.AccountInfo.ContactURIs,
	}

	var resInfo *regInfo
	res, err := c.doReq("POST", regURI, &reqInfo, &resInfo, ctx)
	if err != nil {
		return err
	}

	lg := link.ParseResponse(res)
	if tosLink, ok := lg["terms-of-service"]; ok {
		if resInfo.AgreementURI != tosLink.URI {
			_, ok := c.AccountInfo.AgreementURIs[tosLink.URI]
			if !ok {
				return &AgreementError{tosLink.URI}
			}

			reqInfo.AgreementURI = tosLink.URI
			_, err = c.doReq("POST", regURI, &reqInfo, &resInfo, ctx)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// Load or reload the details of an authorization via the URI.
//
// You can load an authorization from only the URI by creating an Authorization
// with the URI set and then calling this.
func (c *Client) LoadAuthorization(az *Authorization, ctx context.Context) error {
	az.Combinations = nil

	res, err := c.doReq("GET", az.URI, nil, az, ctx)
	if err != nil {
		return err
	}

	err = az.validate()
	if err != nil {
		return err
	}

	az.retryAt = retryAtDefault(res.Header, 10*time.Second)
	return nil
}

// Like LoadAuthorization, but waits the retry time if this is not the first
// attempt to load this authoization. To be used when polling.
func (c *Client) WaitLoadAuthorization(az *Authorization, ctx context.Context) error {
	err := waitUntil(az.retryAt, ctx)
	if err != nil {
		return err
	}

	return c.LoadAuthorization(az, ctx)
}

func (az *Authorization) validate() error {
	/*if az.Resource != "authz" {
		return fmt.Errorf("invalid resource field")
	}*/

	if len(az.Challenges) == 0 {
		return fmt.Errorf("no challenges offered")
	}

	if az.Combinations == nil {
		var is []int
		for i := 0; i < len(az.Challenges); i++ {
			is = append(is, i)
		}
		az.Combinations = append(az.Combinations, is)
	}

	for _, c := range az.Combinations {
		for _, i := range c {
			if i >= len(az.Challenges) {
				return fmt.Errorf("one or more combinations are malformed")
			}
		}
	}

	return nil
}

// Load or reload the details of a challenge via the URI.
//
// You can load a challenge from only the URI by creating a Challenge with the
// URI set and then calling this.
func (c *Client) LoadChallenge(ch *Challenge, ctx context.Context) error {
	res, err := c.doReq("GET", ch.URI, nil, ch, ctx)
	if err != nil {
		return err
	}

	err = ch.validate()
	if err != nil {
		return err
	}

	ch.retryAt = retryAtDefault(res.Header, 10*time.Second)
	return nil
}

// Like LoadChallenge, but waits the retry time if this is not the first
// attempt to load this challenge. To be used when polling.
func (c *Client) WaitLoadChallenge(ch *Challenge, ctx context.Context) error {
	err := waitUntil(ch.retryAt, ctx)
	if err != nil {
		return err
	}

	return c.LoadChallenge(ch, ctx)
}

func (ch *Challenge) validate() error {
	/*if ch.Resource != "challenge" {
		return fmt.Errorf("invalid resource field")
	}*/

	return nil
}

// Create a new authorization for the given hostname.
func (c *Client) NewAuthorization(hostname string, ctx context.Context) (*Authorization, error) {
	di, err := c.getDirectory(ctx)
	if err != nil {
		return nil, err
	}

	az := &Authorization{
		Resource: "new-authz",
		Identifier: Identifier{
			Type:  "dns",
			Value: hostname,
		},
	}

	res, err := c.doReq("POST", di.NewAuthz, az, az, ctx)
	if err != nil {
		return nil, err
	}

	loc := res.Header.Get("Location")
	if res.StatusCode != 201 || !ValidURL(loc) {
		return nil, fmt.Errorf("expected status code 201 and valid Location header: %#v", res)
	}

	az.URI = loc

	err = az.validate()
	if err != nil {
		return nil, err
	}

	return az, nil
}

// Submit a challenge response. Only the challenge URI is required.
//
// The response message is signed with the given key.
//
// If responseKey is nil, the account key is used.
func (c *Client) RespondToChallenge(ch *Challenge, response json.RawMessage, responseKey crypto.PrivateKey, ctx context.Context) error {
	_, err := c.doReqEx("POST", ch.URI, responseKey, &response, c, ctx)
	if err != nil {
		return err
	}

	return nil
}

// Request a certificate using a CSR in DER form.
func (c *Client) RequestCertificate(csrDER []byte, ctx context.Context) (*Certificate, error) {
	di, err := c.getDirectory(ctx)
	if err != nil {
		return nil, err
	}

	crt := &Certificate{
		Resource: "new-cert",
		CSR:      csrDER,
	}

	res, err := c.doReq("POST", di.NewCert, crt, nil, ctx)
	if err != nil {
		return nil, err
	}

	defer res.Body.Close()
	if res.StatusCode != 201 {
		return nil, fmt.Errorf("unexpected status code: %v", res.StatusCode)
	}

	loc := res.Header.Get("Location")
	if !ValidURL(loc) {
		return nil, fmt.Errorf("invalid URI: %#v", loc)
	}

	crt.URI = loc

	err = c.loadCertificate(crt, res, ctx)
	if err != nil {
		return nil, err
	}

	return crt, nil
}

// Load or reload a certificate.
//
// You can load a certificate from its URI by creating a Certificate with the
// URI set and then calling this.
//
// Returns nil if the certificate is not yet ready, but the Certificate field
// will remain nil.
func (c *Client) LoadCertificate(crt *Certificate, ctx context.Context) error {
	res, err := c.doReq("GET", crt.URI, nil, nil, ctx)
	if err != nil {
		return err
	}

	return c.loadCertificate(crt, res, ctx)
}

func (c *Client) loadCertificate(crt *Certificate, res *http.Response, ctx context.Context) error {
	defer res.Body.Close()
	ct := res.Header.Get("Content-Type")
	if ct == "application/pkix-cert" {
		der, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return err
		}

		crt.Certificate = der
		err = c.loadExtraCertificates(crt, res, ctx)
		if err != nil {
			return err
		}

	} else if res.StatusCode == 200 {
		return fmt.Errorf("Certificate returned with unexpected type: %v", ct)
	}

	crt.retryAt = retryAtDefault(res.Header, 10*time.Second)
	return nil
}

func (c *Client) loadExtraCertificates(crt *Certificate, res *http.Response, ctx context.Context) error {
	crt.ExtraCertificates = nil

	for {
		var err error

		lg := link.ParseResponse(res)
		up, ok := lg["up"]
		if !ok {
			return nil
		}

		crtURI, _ := url.Parse(crt.URI)
		upURI, _ := url.Parse(up.URI)
		if crtURI == nil || upURI == nil {
			return fmt.Errorf("invalid URI")
		}
		upURI = crtURI.ResolveReference(upURI)

		res, err = c.doReq("GET", upURI.String(), nil, nil, ctx)
		if err != nil {
			return err
		}

		defer res.Body.Close()
		ct := res.Header.Get("Content-Type")
		if ct != "application/pkix-cert" {
			return fmt.Errorf("unexpected certificate type: %v", ct)
		}

		der, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return err
		}

		res.Body.Close()
		crt.ExtraCertificates = append(crt.ExtraCertificates, der)
	}
}

var closedChannel = make(chan time.Time)

func init() {
	close(closedChannel)
}

// Like LoadCertificate, but waits the retry time if this is not the first
// attempt to load this certificate. To be used when polling.
//
// You will almost certainly want WaitForCertificate instead of this.
func (c *Client) WaitLoadCertificate(crt *Certificate, ctx context.Context) error {
	err := waitUntil(crt.retryAt, ctx)
	if err != nil {
		return err
	}

	return c.LoadCertificate(crt, ctx)
}

// Wait for a pending certificate to be issued. If the certificate has already
// been issued, this is a no-op. Only the URI is required. May be cancelled
// using the context.
func (c *Client) WaitForCertificate(crt *Certificate, ctx context.Context) error {
	for {
		if len(crt.Certificate) > 0 {
			return nil
		}

		err := c.WaitLoadCertificate(crt, ctx)
		if err != nil {
			return err
		}
	}
}

// Wait until time t. If t is before the current time, returns immediately.
// Cancellable via ctx, in which case err is passed through. Otherwise returns
// nil.
func waitUntil(t time.Time, ctx context.Context) error {
	var ch <-chan time.Time
	ch = closedChannel
	now := time.Now()
	if t.After(now) {
		ch = time.After(t.Sub(now))
	}

	// make sure ctx.Done() is checked here even when we are using closedChannel,
	// as select doesn't guarantee any particular priority.
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}

	return nil
}

// Revoke the given certificate.
//
// The revocation key may be the key corresponding to the certificate. If it is
// nil, the account key is used; in this case, the account must be authorized
// for all identifiers in the certificate.
func (c *Client) Revoke(certificateDER []byte, revocationKey crypto.PrivateKey, ctx context.Context) error {
	di, err := c.getDirectory(ctx)
	if err != nil {
		return err
	}

	req := &revokeReq{
		Resource:    "revoke-cert",
		Certificate: certificateDER,
	}

	res, err := c.doReqEx("POST", di.RevokeCert, revocationKey, req, nil, ctx)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	return nil
}

// © 2015 Hugo Landau <hlandau@devever.net>    MIT License
