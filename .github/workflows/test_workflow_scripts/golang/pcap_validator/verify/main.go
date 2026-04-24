// Verifier for Keploy's debug .kpcap files produced while recording the
// pcap-validator sample (samples-go/pcap-validator).
//
// pcap-validator listens on :8080 (HTTP) and :8443 (HTTPS) and persists
// every /touch's label to Postgres and MongoDB. The orchestrator drives
// each listener with a unique marker token in the request body, then
// invokes this verifier on Keploy's incoming + outgoing kpcap files.
//
// Expected behavior:
//
//   HTTP path (plaintext from client → app):
//     • HTTP marker is visible in the INCOMING kpcap (request body).
//     • HTTP marker is visible in the OUTGOING kpcap (PG/Mongo wire).
//
//   HTTPS path (TLS from client → app):
//     • HTTPS marker is NOT visible plaintext in the INCOMING kpcap —
//       Keploy's incoming proxy does not terminate TLS, so the client's
//       TLS records pass through opaquely. (If this ever changes, drop
//       the strict assertion below.)
//     • HTTPS marker IS visible plaintext in the OUTGOING kpcap because
//       the app decrypts the request itself and emits the label to PG /
//       Mongo over plaintext wire — which Keploy's outgoing proxy then
//       captures.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

type event struct {
	Type          string `json:"type"`
	Direction     string `json:"direction"`
	Parser        string `json:"parser"`
	Flow          string `json:"flow"`
	PayloadBase64 string `json:"payload_b64"`
}

type fileSummary struct {
	path       string
	chunks     int
	hasStart   bool
	hasEnd     bool
	markerHits map[string][]hit
}

type hit struct {
	direction string
	parser    string
}

func main() {
	var (
		incoming    = flag.String("incoming", "", "Path to record-incoming-*.kpcap")
		outgoing    = flag.String("outgoing", "", "Path to record-outgoing-*.kpcap")
		httpMarker  = flag.String("http-marker", "", "Marker submitted over HTTP")
		httpsMarker = flag.String("https-marker", "", "Marker submitted over HTTPS")
	)
	flag.Parse()

	missing := []string{}
	for k, v := range map[string]string{
		"-incoming": *incoming, "-outgoing": *outgoing,
		"-http-marker": *httpMarker, "-https-marker": *httpsMarker,
	} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		fail("missing required flags: %s", strings.Join(missing, ", "))
	}

	markers := []string{*httpMarker, *httpsMarker}
	in, err := summarize(*incoming, markers)
	if err != nil {
		fail("incoming: %v", err)
	}
	out, err := summarize(*outgoing, markers)
	if err != nil {
		fail("outgoing: %v", err)
	}

	report(in, out, *httpMarker, *httpsMarker)
}

func summarize(path string, markers []string) (*fileSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	s := &fileSummary{path: path, markerHits: make(map[string][]hit)}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		var ev event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("parse line: %w", err)
		}
		switch ev.Type {
		case "capture-start":
			s.hasStart = true
			continue
		case "capture-end":
			s.hasEnd = true
			continue
		case "chunk":
			// fall through
		default:
			continue
		}
		s.chunks++

		raw, err := base64.StdEncoding.DecodeString(ev.PayloadBase64)
		if err != nil {
			return nil, fmt.Errorf("decode chunk: %w", err)
		}
		text := string(raw)
		for _, m := range markers {
			if strings.Contains(text, m) {
				s.markerHits[m] = append(s.markerHits[m], hit{ev.Direction, ev.Parser})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

func report(in, out *fileSummary, httpMarker, httpsMarker string) {
	printSummary("INCOMING", in)
	printSummary("OUTGOING", out)

	httpInc := len(in.markerHits[httpMarker])
	httpOut := len(out.markerHits[httpMarker])
	httpsInc := len(in.markerHits[httpsMarker])
	httpsOut := len(out.markerHits[httpsMarker])

	fmt.Println("\nresults:")
	fmt.Printf("  HTTP  marker plaintext in INCOMING : %s (hits=%d) — expected YES\n", okmark(httpInc > 0), httpInc)
	fmt.Printf("  HTTP  marker plaintext in OUTGOING : %s (hits=%d) — expected YES\n", okmark(httpOut > 0), httpOut)
	fmt.Printf("  HTTPS marker plaintext in INCOMING : %s (hits=%d) — expected NO  (TLS not terminated by ingress)\n", okmark(httpsInc == 0), httpsInc)
	fmt.Printf("  HTTPS marker plaintext in OUTGOING : %s (hits=%d) — expected YES (app decrypts, hits PG/Mongo plaintext)\n", okmark(httpsOut > 0), httpsOut)

	var problems []string
	if !in.hasStart || !in.hasEnd {
		problems = append(problems, fmt.Sprintf("incoming kpcap missing capture-start/end (start=%v end=%v)", in.hasStart, in.hasEnd))
	}
	if !out.hasStart || !out.hasEnd {
		problems = append(problems, fmt.Sprintf("outgoing kpcap missing capture-start/end (start=%v end=%v)", out.hasStart, out.hasEnd))
	}
	if httpInc == 0 {
		problems = append(problems, "HTTP marker missing from INCOMING — plaintext HTTP capture is broken")
	}
	if httpOut == 0 {
		problems = append(problems, "HTTP marker missing from OUTGOING — outgoing dep capture is broken")
	}
	if httpsOut == 0 {
		problems = append(problems, "HTTPS marker missing from OUTGOING — kpcap is not capturing decrypted dep traffic for the TLS path")
	}
	if httpsInc != 0 {
		// Strict by design: if Keploy ever starts decrypting incoming
		// TLS, this assertion fires loudly so we revisit the test
		// instead of silently letting the behavior drift.
		problems = append(problems, fmt.Sprintf("HTTPS marker found plaintext in INCOMING (%d hits) — incoming TLS appears decrypted; if this is intentional, relax this assertion", httpsInc))
	}

	if len(problems) > 0 {
		fail("validation failed:\n  - %s", strings.Join(problems, "\n  - "))
	}
	fmt.Println("\nOK: kpcap behavior matches expectations for both HTTP and HTTPS")
}

func printSummary(label string, s *fileSummary) {
	fmt.Printf("== %s == %s\n  chunks=%d  capture-start=%v  capture-end=%v\n",
		label, s.path, s.chunks, s.hasStart, s.hasEnd)
	for marker, hits := range s.markerHits {
		fmt.Printf("  marker %s: %d hit(s)\n", marker, len(hits))
		for _, h := range hits {
			fmt.Printf("    - direction=%s parser=%s\n", h.direction, h.parser)
		}
	}
}

func okmark(b bool) string {
	if b {
		return "PASS"
	}
	return "FAIL"
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
