package google

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2/google"
	admin "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/option"

	cfgpkg "github.com/CampusTech/google2snipe/config"
)

// Client wraps the Admin SDK Directory service for ChromeOS devices.
type Client struct {
	svc        *admin.Service
	customerID string
	projection string // "FULL" | "BASIC"
	orgUnit    string
	query      string
	log        *logrus.Logger
}

// New builds an authenticated Client using a service-account key with
// domain-wide delegation impersonating cfg.ImpersonateSubject.
func New(cfg cfgpkg.GoogleConfig, logger *logrus.Logger) (*Client, error) {
	if logger == nil {
		logger = logrus.New()
	}
	keyData, err := os.ReadFile(cfg.CredentialsFile)
	if err != nil {
		return nil, fmt.Errorf("read credentials_file: %w", err)
	}
	jwtCfg, err := google.JWTConfigFromJSON(keyData, admin.AdminDirectoryDeviceChromeosReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("parse service account key: %w", err)
	}
	jwtCfg.Subject = cfg.ImpersonateSubject

	ctx := context.Background()
	svc, err := admin.NewService(ctx, option.WithTokenSource(jwtCfg.TokenSource(ctx)))
	if err != nil {
		return nil, fmt.Errorf("create directory service: %w", err)
	}
	return &Client{
		svc:        svc,
		customerID: cfg.CustomerID,
		projection: strings.ToUpper(cfg.Projection),
		orgUnit:    cfg.OrgUnitPath,
		query:      cfg.Query,
		log:        logger,
	}, nil
}

// ListAllChromeOSDevices pages through every ChromeOS device for the customer.
func (c *Client) ListAllChromeOSDevices(ctx context.Context) ([]Device, error) {
	var out []Device
	pageToken := ""
	for {
		call := c.svc.Chromeosdevices.List(c.customerID).
			Projection(c.projection).
			MaxResults(200).
			Context(ctx)
		if c.orgUnit != "" {
			call = call.OrgUnitPath(c.orgUnit)
		}
		if c.query != "" {
			call = call.Query(c.query)
		}
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, fmt.Errorf("list chromeos devices: %w", err)
		}
		for _, d := range resp.Chromeosdevices {
			dev, err := wrapDevice(d)
			if err != nil {
				return nil, err
			}
			out = append(out, dev)
		}
		c.log.WithField("count", len(out)).Debug("listed chromeos devices page")
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return out, nil
}

// GetDevice fetches a single ChromeOS device by its Google deviceId.
func (c *Client) GetDevice(ctx context.Context, deviceID string) (Device, error) {
	d, err := c.svc.Chromeosdevices.Get(c.customerID, deviceID).
		Projection(c.projection).Context(ctx).Do()
	if err != nil {
		return Device{}, fmt.Errorf("get chromeos device %s: %w", deviceID, err)
	}
	return wrapDevice(d)
}

// About is a lightweight connectivity check: lists a single device page and
// returns the customer ID to confirm the service is reachable.
func (c *Client) About(ctx context.Context) (string, error) {
	_, err := c.svc.Chromeosdevices.List(c.customerID).
		Projection("BASIC").MaxResults(1).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return c.customerID, nil
}
