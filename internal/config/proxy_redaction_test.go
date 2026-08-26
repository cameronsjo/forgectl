package config_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// sentinelProfile is a fully-populated profile whose every value is a distinct
// sentinel, so a leak names which field leaked.
func sentinelProfile() config.ProxyProfile {
	return config.ProxyProfile{
		HTTPProxy:  "opaque-http-value",
		HTTPSProxy: "opaque-https-value",
		AllProxy:   "opaque-all-value",
		NoProxy:    "opaque-no-value",
	}
}

func sentinelValues() []string {
	return []string{"opaque-http-value", "opaque-https-value", "opaque-all-value", "opaque-no-value"}
}

// TestProxyProfile_RendersRedactedUnderEveryVerb makes the never-print
// guarantee a property of the type rather than of the call sites that happen
// to hold one. Before these methods the guarantee was structural — profiles
// were reachable only behind a map the config walk redacts — so a new read
// surface holding a ProxyProfile directly, or any fmt.Errorf("%v", profile),
// would have printed proxy URLs in the clear.
func TestProxyProfile_RendersRedactedUnderEveryVerb(t *testing.T) {
	profile := sentinelProfile()

	buf := &bytes.Buffer{}
	slog.New(slog.NewTextHandler(buf, nil)).Info("selected", "profile", profile)

	marshaled, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	type holder struct{ Profile config.ProxyProfile }
	nested, err := json.Marshal(map[string]config.ProxyProfile{"work": profile})
	if err != nil {
		t.Fatalf("Marshal map: %v", err)
	}
	text, err := profile.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}

	renders := map[string]string{
		"String()":            profile.String(),
		"GoString()":          profile.GoString(),
		"%v":                  fmt.Sprintf("%v", profile),
		"%+v":                 fmt.Sprintf("%+v", profile),
		"%#v":                 fmt.Sprintf("%#v", profile),
		"%s":                  fmt.Sprintf("%s", profile), //nolint:staticcheck // exercising the verb is the assertion
		"%q":                  fmt.Sprintf("%q", profile),
		"%d":                  fmt.Sprintf("%d", profile),
		"wrapped in an error": fmt.Errorf("apply %v: boom", profile).Error(),
		"in a struct, %+v":    fmt.Sprintf("%+v", holder{Profile: profile}),
		"in a map, %v":        fmt.Sprintf("%v", map[string]config.ProxyProfile{"work": profile}),
		"slog":                buf.String(),
		"json":                string(marshaled),
		"json in a map":       string(nested),
		"MarshalText":         string(text),
	}

	for name, rendered := range renders {
		if rendered == "" {
			t.Errorf("%s rendered nothing; this test would pass vacuously", name)
			continue
		}
		if !strings.Contains(rendered, "[redacted]") {
			t.Errorf("%s = %q, which does not carry the redaction marker", name, rendered)
		}
		for _, leak := range sentinelValues() {
			if strings.Contains(rendered, leak) {
				t.Errorf("%s leaked %q: %s", name, leak, rendered)
			}
		}
	}
}

// TestProxyProfile_FieldsStayReadable is the negative control: redaction is a
// rendering property, not erasure. The one sanctioned sink — internal/proxy's
// shell protocol — reads fields directly and must still see the values.
func TestProxyProfile_FieldsStayReadable(t *testing.T) {
	profile := sentinelProfile()
	if profile.HTTPProxy != "opaque-http-value" || profile.NoProxy != "opaque-no-value" {
		t.Fatal("field reads no longer return the configured values")
	}
	if profile.IsZero() {
		t.Fatal("a populated profile reported IsZero")
	}
}
