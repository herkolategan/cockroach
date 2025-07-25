// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package ui embeds the assets for the web UI into the Cockroach binary.
//
// By default, it serves a stub web UI. Linking with distccl will replace the
// stubs with the OSS UI or the CCL UI, respectively. The exported symbols in
// this package are thus function pointers instead of functions so that they can
// be mutated by init hooks.
package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/server/serverpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	_ "github.com/cockroachdb/cockroach/pkg/ui/settings" // Import the settings package to register UI-related settings for doc generation.
	"github.com/cockroachdb/cockroach/pkg/util/httputil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

const (
	cspHeader = "default-src 'self'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"font-src 'self' data:; " +
		"img-src 'self' data:; " +
		"connect-src 'self' https://register.cockroachdb.com;"
)

// TODO(davidh): This setting can be removed after 24.3 since it only
// affects legacy DB page.
var DatabaseLocalityMetadataEnabled = settings.RegisterBoolSetting(
	settings.ApplicationLevel,
	"ui.database_locality_metadata.enabled",
	"if enabled shows extended locality data about databases and tables in DB Console which can be expensive to compute",
	true,
	settings.WithPublic)

// Assets is used for embedded JS assets required for UI.
// In case the binary is built without UI, it provides single index.html file with
// the same content as indexHTML as a fallback.
var Assets fs.FS

// HaveUI tells whether the admin UI has been linked into the binary.
var HaveUI = false

// indexHTML contains HTML which includes the UI JavaScript bundles for
// DB console. It contains a template variable for the nonce attribute to
// provide with the "Content-Security-Policy" header.
var indexHTML = []byte(`<!DOCTYPE html>
<html>
	<head>
		<title>Cockroach Console</title>
		<meta charset="UTF-8">
		<link href="favicon.ico" rel="shortcut icon">
	</head>
	<body>
		<div id="react-layout"></div>
		<script src="bundle.js" type="text/javascript"></script>
	</body>
</html>
`)

type indexHTMLArgs struct {
	// Insecure means disable auth entirely - anyone can use.
	Insecure         bool
	LoggedInUser     *string
	Tag              string
	Version          string
	NodeID           string
	OIDCAutoLogin    bool
	OIDCLoginEnabled bool
	OIDCButtonText   string
	FeatureFlags     serverpb.FeatureFlags

	OIDCGenerateJWTAuthTokenEnabled bool

	LicenseType               string
	SecondsUntilLicenseExpiry int64
	IsManaged                 bool
}

// OIDCUIConf is a variable that stores data required by the
// Admin UI to display and manage the OIDC login flow. It is
// provided by the `oidcAuthenticationServer` at runtime
// since that's where all the OIDC configuration is centralized.
type OIDCUIConf struct {
	ButtonText string
	AutoLogin  bool
	Enabled    bool

	GenerateJWTAuthTokenEnabled bool
}

// OIDCUI is an interface that our OIDC configuration must implement in order to be able
// to pass relevant configuration info to the ui module. This is to pass through variables that
// are necessary to render an appropriate user interface for OIDC support and to set the state
// cookie that OIDC requires for securing auth requests.
type OIDCUI interface {
	GetOIDCConf() OIDCUIConf
}

// bareIndexHTML is used in place of indexHTMLTemplate when the binary is built
// without the web UI.
var bareIndexHTML = []byte(fmt.Sprintf(`<!DOCTYPE html>
<title>CockroachDB</title>
Binary built without web UI.
<hr>
<em>%s</em>`, build.GetInfo().Short()))

// Config contains the configuration parameters for Handler.
type Config struct {
	Insecure bool
	NodeID   *base.NodeIDContainer
	GetUser  func(ctx context.Context) *string
	OIDC     OIDCUI
	Flags    serverpb.FeatureFlags
	Settings *cluster.Settings
}

var uiConfigPath = regexp.MustCompile("^/uiconfig$")

// Handler returns an http.Handler that serves the UI,
// including index.html, which has some login-related variables
// templated into it, as well as static assets.
func Handler(cfg Config) http.Handler {
	// etags is used to provide a unique per-file checksum for each served file,
	// which enables client-side caching using Cache-Control and ETag headers.
	etags := make(map[string]string)
	if HaveUI && Assets != nil {
		// Only compute hashes for UI-enabled builds
		err := httputil.ComputeEtags(Assets, etags)
		if err != nil {
			log.Errorf(context.Background(), "Unable to compute asset hashes: %+v", err)
		}
	}

	fileHandlerChain := httputil.EtagHandler(
		etags,
		http.FileServer(
			http.FS(Assets),
		),
	)
	buildInfo := build.GetInfo()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		licenseType, err := base.LicenseType(cfg.Settings)
		if err != nil {
			log.Errorf(context.Background(), "unable to get license type: %+v", err)
		}
		licenseTTL := base.GetLicenseTTL(r.Context(), cfg.Settings, timeutil.DefaultTimeSource{})
		oidcConf := cfg.OIDC.GetOIDCConf()
		major, minor := build.BranchReleaseSeries()
		args := indexHTMLArgs{
			Insecure:         cfg.Insecure,
			LoggedInUser:     cfg.GetUser(r.Context()),
			Tag:              buildInfo.Tag,
			Version:          fmt.Sprintf("v%d.%d", major, minor),
			OIDCAutoLogin:    oidcConf.AutoLogin,
			OIDCLoginEnabled: oidcConf.Enabled,
			OIDCButtonText:   oidcConf.ButtonText,
			FeatureFlags:     cfg.Flags,

			OIDCGenerateJWTAuthTokenEnabled: oidcConf.GenerateJWTAuthTokenEnabled,

			LicenseType:               licenseType,
			SecondsUntilLicenseExpiry: licenseTTL,
			// log.RedactionPolicyManaged is set to true only when cluster is running under
			// managed service control.
			IsManaged: log.RedactionPolicyManaged,
		}
		if cfg.NodeID != nil {
			args.NodeID = cfg.NodeID.String()
		}
		if uiConfigPath.MatchString(r.URL.Path) {
			argBytes, err := json.Marshal(args)
			if err != nil {
				log.Errorf(r.Context(), "unable to deserialize ui config args: %v", err)
				http.Error(w, err.Error(), 500)
				return
			}
			_, err = w.Write(argBytes)
			if err != nil {
				log.Errorf(r.Context(), "unable to write ui config args: %v", err)
				http.Error(w, err.Error(), 500)
				return
			}
			return
		}

		if r.Header.Get("Crdb-Development") != "" {
			w.Header().Set("Content-Security-Policy", cspHeader)
			http.ServeContent(w, r, "index.html", buildInfo.GoTime(), bytes.NewReader(indexHTML))
			return
		}

		if !HaveUI {
			http.ServeContent(w, r, "index.html", buildInfo.GoTime(), bytes.NewReader(bareIndexHTML))
			return
		}

		if r.URL.Path != "/" {
			fileHandlerChain.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Security-Policy", cspHeader)
		http.ServeContent(w, r, "index.html", buildInfo.GoTime(), bytes.NewReader(indexHTML))
	})
}
