package family

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The table above is imported from zigbee-herdsman's adapterDiscovery.ts: they have the user
// base and the hardware, we have neither and will not. The risk that creates is staleness —
// theirs moves, adapters get added, flags get corrected — and a copy nobody re-reads is worse
// than no copy, because it looks authoritative while being wrong.
//
// So this is the mechanism that keeps the claim honest, in the same spirit as the CI gate on
// commit trailers: it reads their file and reports what we are missing or disagree about. It is
// deliberately a *report*, not a failure — their table is not our specification, and a new
// adapter of theirs is a decision for a human (which zigpy radio_type? can we serve its flow
// control?) rather than something to auto-adopt.
//
// ⚠️ **It needs no download, which is what lets it run at all** — and a check that never runs is
// exactly the copy-nobody-re-reads this exists to prevent. The dev shell already carries a copy:
// nixpkgs installs zigbee2mqtt's node_modules complete (which is what the herdsman client gate
// runs on), and the *compiled* `dist/adapter/adapterDiscovery.js` keeps the source's exact
// formatting, comments and all, so parseHerdsman reads it unchanged. So the default is that
// file, found through NODE_PATH, and the check runs per commit — CI drives `go test` through
// `nix develop` too. HERDSMAN_ADAPTERS overrides, which is how you check against a version newer
// than the pinned package:
//
//	curl -o /tmp/adapterDiscovery.ts \
//	  https://raw.githubusercontent.com/Koenkk/zigbee-herdsman/master/src/adapter/adapterDiscovery.ts
//	HERDSMAN_ADAPTERS=/tmp/adapterDiscovery.ts go test ./internal/family/ -run Drift -v
//
// It skips where neither is available rather than failing — a cross-built test binary run on a
// Windows machine has no dev shell and no store, and that is not drift.
func TestDriftAgainstHerdsmanAdapterTable(t *testing.T) {
	path := os.Getenv("HERDSMAN_ADAPTERS")
	if path == "" {
		path = herdsmanInNodePath()
	}
	if path == "" {
		t.Skip("no copy of zigbee-herdsman's adapter table: no NODE_PATH with zigbee-herdsman in it, " +
			"and HERDSMAN_ADAPTERS unset — run this under `nix develop`, or point HERDSMAN_ADAPTERS at a copy")
	}
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	t.Logf("read %s", path)

	theirs := parseHerdsman(string(source))
	if len(theirs) == 0 {
		t.Fatal("parsed no entries; adapterDiscovery.ts has changed shape and this check needs rewriting")
	}
	t.Logf("herdsman carries %d entries; we carry %d rows", len(theirs), len(table))

	for _, e := range theirs {
		switch e.family {
		case "zboss":
			// Deliberately not imported: zboss is not a zigpy RadioType, so there is no legal
			// value for the TXT record and ZHA would drop the advert.
			continue
		case "zigate":
			// Deliberately not imported: zigate would be a fourth family, which is a decision
			// rather than a table row.
			continue
		}
		if !weCover(e) {
			t.Errorf("herdsman has %s:%s %q (%s) and we do not — decide whether to carry it",
				e.vendor, e.product, e.pathRegex, e.family)
		}
	}
}

// herdsmanInNodePath finds the copy the dev shell installs, or "" if there is none. NODE_PATH
// is a path LIST, so it is split rather than used whole: a shell that has appended to it would
// otherwise turn this check off silently, which is the one failure mode it must not have.
func herdsmanInNodePath() string {
	for _, dir := range filepath.SplitList(os.Getenv("NODE_PATH")) {
		p := filepath.Join(dir, "zigbee-herdsman", "dist", "adapter", "adapterDiscovery.js")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

type herdsmanEntry struct {
	family       string
	vendor       string
	product      string
	manufacturer string
	pathRegex    string
	rtscts       bool
	baud         string
}

var (
	familyLine  = regexp.MustCompile(`^\s{4}(\w+): \[`)
	vendorLine  = regexp.MustCompile(`^\s+vendorId: "([^"]*)"`)
	productLine = regexp.MustCompile(`^\s+productId: "([^"]*)"`)
	makerLine   = regexp.MustCompile(`^\s+manufacturer: "([^"]*)"`)
	regexLine   = regexp.MustCompile(`^\s+pathRegex: "([^"]*)"`)
	optionsLine = regexp.MustCompile(`^\s+options: \{(.*)\}`)
)

// parseHerdsman reads the entries out of the TypeScript by shape rather than by parsing it.
// Commented-out stubs are skipped, which is why vendor and product are required to be present.
func parseHerdsman(source string) []herdsmanEntry {
	var out []herdsmanEntry
	var fam string
	var cur herdsmanEntry
	for _, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if m := familyLine.FindStringSubmatch(line); m != nil {
			fam = m[1]
			continue
		}
		if m := vendorLine.FindStringSubmatch(line); m != nil {
			cur = herdsmanEntry{family: fam, vendor: m[1]}
			continue
		}
		if m := productLine.FindStringSubmatch(line); m != nil {
			cur.product = m[1]
			continue
		}
		if m := makerLine.FindStringSubmatch(line); m != nil {
			cur.manufacturer = m[1]
			continue
		}
		if m := optionsLine.FindStringSubmatch(line); m != nil {
			cur.rtscts = strings.Contains(m[1], "rtscts: true")
			if i := strings.Index(m[1], "baudRate: "); i >= 0 {
				cur.baud = strings.TrimSuffix(strings.Fields(m[1][i+len("baudRate: "):])[0], ",")
			}
		}
		if m := regexLine.FindStringSubmatch(line); m != nil {
			cur.pathRegex = m[1]
			if cur.vendor != "" && cur.product != "" {
				out = append(out, cur)
			}
			cur = herdsmanEntry{}
		}
	}
	return out
}

// weCover reports whether some row of ours answers for the same adapter.
//
// Their pathRegex matches a by-id path and our match is a descriptor substring, so the two
// cannot be compared directly. What is compared is the *distinguishing* words: their regex
// minus the brand, since the manufacturer is carried separately in their entry and is not what
// tells two of a vendor's sticks apart. Every remaining word must appear in our row, which is
// what stops `.*sonoff.*max.*` being reported as covered by the ZBDongle-P row.
func weCover(e herdsmanEntry) bool {
	brand := map[string]bool{}
	for _, w := range words(e.manufacturer) {
		brand[w] = true
	}
	var distinguishing []string
	for _, w := range words(stripLookahead(e.pathRegex)) {
		if len(w) >= 2 && !brand[w] {
			distinguishing = append(distinguishing, w)
		}
	}

	for _, r := range table {
		if !strings.EqualFold(r.vendor, e.vendor) || !strings.EqualFold(r.product, e.product) {
			continue
		}
		// The row's own words: the match string, plus the human name, which carries the model
		// where the match string carries only enough to be unambiguous.
		haystack := strings.ToLower(r.match + " " + r.name)
		covered := true
		for _, w := range distinguishing {
			if !strings.Contains(haystack, w) {
				covered = false
				break
			}
		}
		if covered {
			return true
		}
	}
	return false
}

var (
	lookahead   = regexp.MustCompile(`\(\?![^)]*\)`)
	nonWordRune = regexp.MustCompile(`[^a-z0-9.-]+`)
)

func stripLookahead(s string) string { return lookahead.ReplaceAllString(s, " ") }

// words splits an identifier into lowercase tokens, keeping the hyphens and dots that are part
// of model names — "zbt-1" and "slae.sh" are one word each, not three.
func words(s string) []string {
	var out []string
	for _, w := range nonWordRune.Split(strings.ToLower(strings.ReplaceAll(s, `\`, "")), -1) {
		if w = strings.Trim(w, ".-"); w != "" {
			out = append(out, w)
		}
	}
	return out
}
