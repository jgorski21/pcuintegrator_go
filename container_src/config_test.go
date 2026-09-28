package main

import (
	"strings"
	"testing"
)

// setRequiredEnv provides what LoadConfig refuses to start without, and blanks
// every DeliverIT key so the developer's shell cannot leak into the test.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLICKUP_TOKEN", "pk_test")
	t.Setenv("PRODUCTIVE_TOKEN", "prod_test")
	t.Setenv("PRODUCTIVE_ORG_ID", "org1")
	for _, key := range []string{"DELIVERIT_BASE_URL", "DELIVERIT_API_KEY", "DELIVERIT_API_KEY_PREFIX", "DELIVERIT_RPS"} {
		t.Setenv(key, "")
	}
}

func TestLoadConfigDeliverITDefaults(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DeliverITBaseURL != "" || cfg.DeliverITAPIKey != "" || cfg.DeliverITKeyPrefix != "" {
		t.Fatalf("DeliverIT must be off by default: %+v", cfg)
	}
	if cfg.DeliverITRPS != 2 {
		t.Fatalf("DELIVERIT_RPS default = %v, want 2", cfg.DeliverITRPS)
	}
	if setup := cfg.deliverITSetup(); setup.Enabled || setup.DisabledReason == "" {
		t.Fatalf("setup = %+v", setup)
	}
}

func TestLoadConfigReadsAndTrimsDeliverITKeys(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DELIVERIT_BASE_URL", " https://internal.deliverit.pl ")
	t.Setenv("DELIVERIT_API_KEY", testDeliverITKey+"\r\n") // env file with CRLF
	t.Setenv("DELIVERIT_RPS", "0.5")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DeliverITBaseURL != "https://internal.deliverit.pl" || cfg.DeliverITAPIKey != testDeliverITKey || cfg.DeliverITRPS != 0.5 {
		t.Fatalf("cfg = %q %q %v", cfg.DeliverITBaseURL, cfg.DeliverITAPIKey, cfg.DeliverITRPS)
	}
	if setup := cfg.deliverITSetup(); !setup.Enabled || setup.Proxied || setup.KeyLabel != "dit_Ab3dE5gH…" {
		t.Fatalf("setup = %+v", setup)
	}
}

// DELIVERIT_BASE_URL is a deploy-time var: a typo fails the start like every other
// validated var, instead of silently sending the key somewhere else.
func TestLoadConfigRejectsAnInvalidDeliverITBaseURL(t *testing.T) {
	for _, base := range []string{"http://internal.deliverit.pl", "internal.deliverit.pl", "https://u:p@internal.deliverit.pl"} {
		setRequiredEnv(t)
		t.Setenv("DELIVERIT_BASE_URL", base)
		_, err := LoadConfig()
		if err == nil || !strings.Contains(err.Error(), "DELIVERIT_BASE_URL") {
			t.Fatalf("%q: want a DELIVERIT_BASE_URL error, got %v", base, err)
		}
		if strings.Contains(err.Error(), "u:p") {
			t.Fatalf("the error must not echo credentials: %v", err)
		}
	}
}

// A DeliverIT key pasted into DELIVERIT_BASE_URL (or glued onto a URL) must not come
// back in the start-up error: that error lands in the container log. The message
// names the variable, never its value.
func TestDeliverITConfigErrorsNeverQuoteTheValue(t *testing.T) {
	for _, base := range []string{
		strayDeliverITKey,
		"http://" + strayDeliverITKey,
		"https://" + strayDeliverITKey + "@internal.deliverit.pl",
		"https://internal.deliverit.pl/?key=" + strayDeliverITKey,
		"https://internal.deliverit.pl/#" + strayDeliverITKey,
		"https://deliverit.internal/" + strayDeliverITKey,
	} {
		setRequiredEnv(t)
		t.Setenv("DELIVERIT_BASE_URL", base)
		_, err := LoadConfig()
		if err == nil || !strings.Contains(err.Error(), "DELIVERIT_BASE_URL") {
			t.Fatalf("%q: want an error naming DELIVERIT_BASE_URL, got %v", base, err)
		}
		if strings.Contains(err.Error(), strayDeliverITKey) {
			t.Fatalf("the start-up error quotes the value: %v", err)
		}

		cfg := testConfig("http://x/", "http://y/")
		cfg.DeliverITBaseURL = base
		setup := cfg.deliverITSetup()
		for _, s := range []string{setup.Error, setup.Warning, setup.DisabledReason} {
			if strings.Contains(s, strayDeliverITKey) {
				t.Fatalf("the setup quotes the value: %q", s)
			}
		}
		if !strings.Contains(setup.Error, "DELIVERIT_BASE_URL") {
			t.Fatalf("%q: the setup error must name the variable: %+v", base, setup)
		}
	}

	setRequiredEnv(t)
	t.Setenv("DELIVERIT_RPS", "100")
	_, err := LoadConfig()
	if err == nil || err.Error() != "DELIVERIT_RPS must be in (0, 4] — at least 250 ms between requests to DeliverIT" {
		t.Fatalf("DELIVERIT_RPS: %v", err)
	}
}

func TestLoadConfigValidatesDeliverITRPS(t *testing.T) {
	for value, ok := range map[string]bool{"4": true, "2": true, "0.1": true, "0": false, "-1": false, "4.5": false, "100": false} {
		setRequiredEnv(t)
		t.Setenv("DELIVERIT_RPS", value)
		_, err := LoadConfig()
		if ok && err != nil {
			t.Fatalf("DELIVERIT_RPS=%s: %v", value, err)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), "DELIVERIT_RPS")) {
			t.Fatalf("DELIVERIT_RPS=%s must be rejected, got %v", value, err)
		}
	}
}

// Unlike CLICKUP_TOKEN / PRODUCTIVE_TOKEN, a missing or broken DeliverIT key never
// stops the process: the ClickUp -> Productive sync must keep running.
func TestLoadConfigNeverFailsOnTheDeliverITKey(t *testing.T) {
	for _, key := range []string{"", "garbage", "dit_", "dit_a b"} {
		setRequiredEnv(t)
		t.Setenv("DELIVERIT_BASE_URL", "https://internal.deliverit.pl")
		t.Setenv("DELIVERIT_API_KEY", key)
		if _, err := LoadConfig(); err != nil {
			t.Fatalf("key %q blocked the start: %v", key, err)
		}
	}
	setRequiredEnv(t)
	t.Setenv("DELIVERIT_BASE_URL", "http://deliverit.internal")
	t.Setenv("DELIVERIT_API_KEY_PREFIX", "invalid")
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("a malformed Worker secret blocked the start: %v", err)
	}
}

func TestDeliverITSetup(t *testing.T) {
	type want struct {
		enabled, proxied bool
		key, label       string
		warning, error   string // substrings; "" = none expected
	}
	cases := []struct {
		name              string
		base, key, prefix string
		want              want
	}{
		{name: "off", base: "", want: want{}},
		{name: "direct", base: "https://internal.deliverit.pl", key: testDeliverITKey,
			want: want{enabled: true, key: testDeliverITKey, label: "dit_Ab3dE5gH…"}},
		{name: "direct without key", base: "https://internal.deliverit.pl",
			want: want{warning: "DELIVERIT_API_KEY is not set"}},
		{name: "direct with malformed key", base: "https://internal.deliverit.pl", key: "pk_oops",
			want: want{error: "DELIVERIT_API_KEY is malformed"}},
		{name: "proxied", base: "http://deliverit.internal", prefix: "dit_Ab3dE5gH",
			want: want{enabled: true, proxied: true, label: "dit_Ab3dE5gH…"}},
		{name: "proxied without Worker secret", base: "http://deliverit.internal",
			want: want{proxied: true, warning: "wrangler secret put DELIVERIT_API_KEY"}},
		{name: "proxied with malformed Worker secret", base: "http://deliverit.internal", prefix: "invalid",
			want: want{proxied: true, error: "malformed"}},
		{name: "proxied ignores a key in the container env", base: "http://deliverit.internal",
			key: testDeliverITKey, prefix: "dit_Ab3dE5gH",
			want: want{enabled: true, proxied: true, label: "dit_Ab3dE5gH…", warning: "ignored"}},
		{name: "invalid base url", base: "http://internal.deliverit.pl", key: testDeliverITKey,
			want: want{error: "DELIVERIT_BASE_URL"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig("http://x/", "http://y/")
			cfg.DeliverITBaseURL, cfg.DeliverITAPIKey, cfg.DeliverITKeyPrefix = tc.base, tc.key, tc.prefix
			got := cfg.deliverITSetup()

			if got.Enabled != tc.want.enabled || got.Proxied != tc.want.proxied || got.APIKey != tc.want.key ||
				got.KeyLabel != tc.want.label {
				t.Fatalf("setup = %+v, want %+v", got, tc.want)
			}
			if !got.Enabled && got.DisabledReason == "" {
				t.Fatal("a disabled stage must say why")
			}
			if !containsOrEmpty(got.Warning, tc.want.warning) || !containsOrEmpty(got.Error, tc.want.error) {
				t.Fatalf("warning=%q error=%q, want %q / %q", got.Warning, got.Error, tc.want.warning, tc.want.error)
			}
			for _, s := range []string{got.Warning, got.Error, got.DisabledReason, got.KeyLabel} {
				if strings.Contains(s, testDeliverITKey) {
					t.Fatalf("the key leaked into %q", s)
				}
			}
		})
	}
}

func containsOrEmpty(got, want string) bool {
	if want == "" {
		return got == ""
	}
	return strings.Contains(got, want)
}
