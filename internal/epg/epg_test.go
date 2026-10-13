package epg

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const guide = `<?xml version="1.0" encoding="UTF-8"?>
<tv>
<channel id="cnn.us"><display-name>CNN</display-name><icon src="http://i/cnn.png"/></channel>
<channel id="bbc.uk"><display-name>BBC One</display-name></channel>
<programme start="20991231100000 +0000" stop="20991231110000 +0000" channel="cnn.us"><title>News &amp; More</title></programme>
<programme start="20000101100000 +0000" stop="20000101110000 +0000" channel="cnn.us"><title>Old</title></programme>
<programme start="20991231100000 +0000" stop="20991231110000 +0000" channel="bbc.uk"><title>Unmapped</title></programme>
</tv>`

func TestScanAndGenerate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.xml")
	if err := os.WriteFile(p, []byte(guide), 0o644); err != nil {
		t.Fatal(err)
	}
	chans, err := ScanChannels(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(chans) != 2 || chans[0].ID != "cnn.us" || chans[0].Icon != "http://i/cnn.png" {
		t.Fatalf("channels = %+v", chans)
	}
	var buf bytes.Buffer
	err = Generate(&buf, Plan{
		Channels: []OutChannel{{ID: "1001", Name: "CNN HD"}, {ID: "1002", Name: "Dummy <1>"}},
		Map:      map[string][]string{"cnn.us": {"1001"}},
		Dummy:    []OutChannel{{ID: "1002", Name: "Dummy <1>"}},
		Files:    []File{{Path: p, Owns: func(string) bool { return true }}},
		Now:      time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{`<channel id="1001">`, `channel="1001"><title>News &amp; More</title>`, `Dummy &lt;1&gt;`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	for _, bad := range []string{"Unmapped", ">Old<", "cnn.us"} {
		if strings.Contains(out, bad) {
			t.Errorf("output should not contain %q", bad)
		}
	}
}
