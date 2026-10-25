// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package omada

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Device is an adopted Omada device (gateway, switch or AP). Verified against a
// live v6.2 controller (GET /sites/{id}/devices). Only stable inventory fields
// are modelled; the endpoint returns many more runtime/telemetry keys.
type Device struct {
	Name            string `json:"name"`
	Type            string `json:"type"`  // "gateway" | "switch" | "ap"
	Model           string `json:"model"` // e.g. "ES205GP"
	CompoundModel   string `json:"compoundModel"`
	MAC             string `json:"mac"`
	SN              string `json:"sn"`
	IP              string `json:"ip"`
	Status          int    `json:"status"`
	StatusCategory  int    `json:"statusCategory"` // 0=disconnected, 1=connected, 2=pending
	FirmwareVersion string `json:"firmwareVersion"`
	Version         string `json:"version"`
	NeedUpgrade     bool   `json:"needUpgrade"`
	UptimeSeconds   int64  `json:"uptimeLong"`
	ClientNum       int    `json:"clientNum"`
	HWVersion       string `json:"hwVersion"`
}

func devicesPath(siteID string) string {
	return fmt.Sprintf("/sites/%s/devices", siteID)
}

// ListDevices returns all adopted devices on a site.
//
// This endpoint returns a bare JSON array in `result` (not the paginated
// {data,totalRows} envelope the /setting/* list endpoints use), so it is decoded
// directly rather than through the pager.
func (c *Client) ListDevices(ctx context.Context, siteID string) ([]Device, error) {
	var devices []Device
	if err := c.Do(ctx, "GET", devicesPath(siteID), nil, &devices); err != nil {
		return nil, fmt.Errorf("listing devices: %w", err)
	}
	return devices, nil
}

// switchPath is a switch's own document on the web API.
func switchPath(siteID, mac string) string {
	return fmt.Sprintf("/sites/%s/switches/%s", siteID, NormalizeMAC(mac))
}

// ErrDeviceNotFound is returned by GetDevice when the MAC is not in the site's
// inventory, so callers can tell a removed device apart from a failed request.
var ErrDeviceNotFound = errors.New("device not found")

// GetDevice returns one adopted device by MAC from the site's inventory. A MAC
// that is not in the inventory returns ErrDeviceNotFound: the device was never
// adopted, or has been removed from the site.
func (c *Client) GetDevice(ctx context.Context, siteID, mac string) (*Device, error) {
	devices, err := c.ListDevices(ctx, siteID)
	if err != nil {
		return nil, err
	}
	want := NormalizeMAC(mac)
	for i := range devices {
		if NormalizeMAC(devices[i].MAC) == want {
			return &devices[i], nil
		}
	}
	return nil, fmt.Errorf("%w: site %q has no adopted device %s", ErrDeviceNotFound, siteID, want)
}

// RenameSwitch sets a switch's display name:
//
//	PATCH /sites/{site}/switches/{mac}   {"name": "..."}
//
// This is the request the controller UI sends. Unlike the per-port route it
// accepts a partial body, and nothing else on the device changes. Verified on
// ES206X-M2 and ES210X-M2 switches under a v5.15 controller.
func (c *Client) RenameSwitch(ctx context.Context, siteID, mac, name string) error {
	if err := c.Do(ctx, http.MethodPatch, switchPath(siteID, mac), map[string]any{"name": name}, nil); err != nil {
		return fmt.Errorf("renaming switch %s: %w", mac, err)
	}
	return nil
}
