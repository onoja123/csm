package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestBannerShowsTheLogoAndSetup(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	var out bytes.Buffer
	printBanner(&out, t.TempDir())

	text := out.String()
	if strings.Contains(text, "\x1b[") {
		t.Fatalf("NO_COLOR banner has escape codes:\n%s", text)
	}

	for _, want := range []string{"█", "csm  v" + version, "accounts   none yet", "service    not running", "sentry     not connected", "csm help"} {
		if !strings.Contains(text, want) {
			t.Errorf("banner is missing %q:\n%s", want, text)
		}
	}

	if got := len(renderLogo(palette{})); got != len(logoPixels)/2 {
		t.Errorf("logo is %d lines, want %d", got, len(logoPixels)/2)
	}
}

func TestLogoRowsLineUp(t *testing.T) {
	for i, row := range logoPixels {
		if len(row) != len(logoPixels[0]) {
			t.Errorf("logo row %d is %d wide, want %d", i, len(row), len(logoPixels[0]))
		}
	}
}
