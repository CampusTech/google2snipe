package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/googleapi"
	licensing "google.golang.org/api/licensing/v1"
	"google.golang.org/api/option"

	cfgpkg "github.com/CampusTech/google2snipe/config"
)

// LicenseAssignment holds a single user↔license assignment returned by the
// Enterprise License Manager API.
type LicenseAssignment struct {
	UserEmail string
	ProductID string
	SKUID     string
	SKUName   string
}

// LicensingClient wraps the licensing/v1 service.
type LicensingClient struct {
	svc        *licensing.Service
	customerID string
	log        *logrus.Logger
}

// NewLicensingClient builds a licensing/v1 client via the SA + DWD, reusing the
// debug transport. customerID is the Workspace customer domain or unique id;
// "" derives the domain from the impersonation subject.
func NewLicensingClient(cfg cfgpkg.GoogleConfig, customerID string, logger *logrus.Logger) (*LicensingClient, error) {
	if logger == nil {
		logger = logrus.New()
	}
	if customerID == "" {
		if at := strings.LastIndex(cfg.ImpersonateSubject, "@"); at >= 0 {
			customerID = cfg.ImpersonateSubject[at+1:]
		}
	}
	if customerID == "" {
		return nil, fmt.Errorf("licensing customerID is empty and could not be derived from impersonate_subject %q (no domain) — set licenses.workspace.customer_id", cfg.ImpersonateSubject)
	}
	keyData, err := os.ReadFile(cfg.CredentialsFile)
	if err != nil {
		return nil, fmt.Errorf("read credentials_file: %w", err)
	}
	jwtCfg, err := google.JWTConfigFromJSON(keyData, licensing.AppsLicensingScope)
	if err != nil {
		return nil, fmt.Errorf("parse service account key: %w", err)
	}
	jwtCfg.Subject = cfg.ImpersonateSubject
	ctx := context.Background()
	httpClient := jwtCfg.Client(ctx)
	httpClient.Transport = &debugTransport{base: httpClient.Transport, log: logger}
	svc, err := licensing.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("create licensing service: %w", err)
	}
	return &LicensingClient{svc: svc, customerID: customerID, log: logger}, nil
}

// isServiceDisabled reports whether err indicates the Licensing API is not enabled
// / not accessible for the project (a global setup failure, not a per-product gap).
func isServiceDisabled(err error) bool {
	s := err.Error()
	return strings.Contains(s, "SERVICE_DISABLED") ||
		strings.Contains(s, "accessNotConfigured") ||
		strings.Contains(s, "has not been used in project") ||
		strings.Contains(s, "it is disabled")
}

// ListAssignments pages through every license assignment for each product.
// Genuine "customer not entitled to this product" 403/404s are skipped (expected
// when probing a candidate product list). A SERVICE_DISABLED / access-not-configured
// 403 (the Licensing API not enabled for the service account's project) is surfaced
// as a hard, actionable error. If EVERY product 403/404s and nothing is returned,
// that is almost certainly a global setup problem (scope not granted, or the subject
// lacks license-admin rights), so it is logged at Warn.
func (c *LicensingClient) ListAssignments(ctx context.Context, products []string) ([]LicenseAssignment, error) {
	var out []LicenseAssignment
	skipped := 0
	for _, product := range products {
		pageToken := ""
		for {
			call := c.svc.LicenseAssignments.ListForProduct(product, c.customerID).MaxResults(1000).Context(ctx)
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			resp, err := call.Do()
			if err != nil {
				var gerr *googleapi.Error
				if errors.As(err, &gerr) {
					if isServiceDisabled(err) {
						return nil, fmt.Errorf("Google Licensing API is not enabled for the service account's project — enable licensing.googleapis.com and retry: %w", err)
					}
					if gerr.Code == 403 || gerr.Code == 404 {
						c.log.WithField("product", product).WithField("code", gerr.Code).Debug("skipping product (customer not entitled)")
						skipped++
						break
					}
				}
				return nil, fmt.Errorf("list assignments for %s: %w", product, err)
			}
			for _, a := range resp.Items {
				out = append(out, LicenseAssignment{
					UserEmail: a.UserId, ProductID: a.ProductId, SKUID: a.SkuId, SKUName: a.SkuName,
				})
			}
			if resp.NextPageToken == "" {
				break
			}
			pageToken = resp.NextPageToken
		}
	}
	if len(out) == 0 && skipped > 0 && skipped == len(products) {
		c.log.WithField("products", len(products)).Warn("all probed products returned 403/404 with zero assignments — likely a setup issue (apps.licensing scope not granted to the service account, or the impersonated subject lacks license-management admin rights)")
	}
	return out, nil
}

// SerializeAssignments marshals the assignment list to indented JSON.
func SerializeAssignments(a []LicenseAssignment) ([]byte, error) {
	return json.MarshalIndent(a, "", "  ")
}

// DeserializeAssignments unmarshals a JSON-encoded assignment list.
func DeserializeAssignments(data []byte) ([]LicenseAssignment, error) {
	var a []LicenseAssignment
	return a, json.Unmarshal(data, &a)
}
