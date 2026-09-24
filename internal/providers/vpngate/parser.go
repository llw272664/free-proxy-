// Package vpngate discovers OpenVPN nodes from the public VPNGate endpoint.
package vpngate

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/masteralanlab/free-proxy/internal/domain"
)

// ParseStats records CSV parsing counters for diagnostics.
type ParseStats struct {
	TotalRows        int
	ValidRows        int
	DuplicateRows    int
	MalformedRows    int
	MissingFieldRows int
}

// ParseResult bundles parsed nodes and stats.
type ParseResult struct {
	Nodes []domain.DiscoveredNode
	Stats ParseStats
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]+`)

func safeInt(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0
	}
	return n
}

func normalizeTransport(v string) domain.TransportProtocol {
	l := strings.ToLower(v)
	switch {
	case strings.Contains(l, "tcp"):
		return domain.TransportTCP
	case strings.Contains(l, "udp"):
		return domain.TransportUDP
	default:
		return domain.TransportUnknown
	}
}

func parseRemote(configText, fallbackHost string) (string, int, domain.TransportProtocol) {
	remoteHost := fallbackHost
	remotePort := 0
	transport := domain.TransportUnknown
	for _, raw := range strings.Split(configText, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		switch strings.ToLower(parts[0]) {
		case "proto":
			if len(parts) >= 2 {
				transport = normalizeTransport(parts[1])
			}
		case "remote":
			if len(parts) >= 3 {
				remoteHost = parts[1]
				remotePort = safeInt(parts[2])
				if len(parts) >= 4 {
					transport = normalizeTransport(parts[3])
				}
			}
		}
	}
	return remoteHost, remotePort, transport
}

func decodeConfig(encoded string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		// try raw/url variants leniently
		data, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return "", fmt.Errorf("invalid OpenVPN configuration")
		}
	}
	return string(data), nil
}

func buildNodeID(countryCode, ipAddress string) string {
	sum := sha256.Sum256([]byte("vpngate:" + ipAddress))
	digest := hex.EncodeToString(sum[:])[:12]
	prefix := nonAlnum.ReplaceAllString(strings.ToUpper(countryCode), "")
	if prefix == "" {
		prefix = "XX"
	}
	return strings.ToLower(prefix) + "-" + digest
}

func providerIdentity(ipAddress string) string {
	return "vpngate:" + strings.TrimSpace(ipAddress)
}

// ParseResponse parses the VPNGate CSV, keeping at most limit nodes.
func ParseResponse(text string, limit int, now time.Time) (ParseResult, error) {
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimRight(l, "\r")
		if l == "" || strings.HasPrefix(l, "*") {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return ParseResult{}, fmt.Errorf("VPNGate response is empty or has no header")
	}
	lines[0] = strings.TrimPrefix(lines[0], "#")
	if !strings.Contains(lines[0], "IP") || !strings.Contains(lines[0], "OpenVPN_ConfigData_Base64") {
		return ParseResult{}, fmt.Errorf("VPNGate response has no usable header")
	}

	headerReader := csv.NewReader(strings.NewReader(lines[0]))
	headerReader.FieldsPerRecord = -1
	header, err := headerReader.Read()
	if err != nil {
		return ParseResult{}, fmt.Errorf("VPNGate header unreadable: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	get := func(row []string, name string) string {
		if idx, ok := col[name]; ok && idx < len(row) {
			return row[idx]
		}
		return ""
	}

	var result ParseResult
	seen := map[string]bool{}
	// Keep the positions of the fields needed to build a node.  VPNGate
	// publishes one record per physical line, so parsing each line separately
	// lets us recover when one dirty record contains an otherwise harmless
	// quote.  A malformed record is then isolated instead of consuming the
	// remainder of the response as an unterminated quoted field would do.
	required := make([]int, 0, 2)
	for _, name := range []string{"IP", "OpenVPN_ConfigData_Base64"} {
		if idx, ok := col[name]; ok {
			required = append(required, idx)
		}
	}
	var firstCSVErr error
	parsedRows := 0
	for _, line := range lines[1:] {
		result.Stats.TotalRows++
		row, err := parseRecord(line, required)
		if err != nil {
			result.Stats.MalformedRows++
			if firstCSVErr == nil {
				firstCSVErr = err
			}
			continue
		}
		parsedRows++
		if len(result.Nodes) >= limit {
			continue
		}
		ipAddress := strings.TrimSpace(get(row, "IP"))
		encoded := strings.TrimSpace(get(row, "OpenVPN_ConfigData_Base64"))
		if ipAddress == "" || encoded == "" {
			result.Stats.MissingFieldRows++
			continue
		}
		configText, err := decodeConfig(encoded)
		if err != nil {
			result.Stats.MalformedRows++
			continue
		}
		remoteHost, remotePort, transport := parseRemote(configText, ipAddress)
		if remotePort == 0 {
			result.Stats.MalformedRows++
			continue
		}
		identity := providerIdentity(ipAddress)
		if seen[identity] {
			result.Stats.DuplicateRows++
			continue
		}
		countryCode := strings.ToUpper(strings.TrimSpace(get(row, "CountryShort")))
		result.Nodes = append(result.Nodes, domain.DiscoveredNode{
			ID:               buildNodeID(countryCode, ipAddress),
			Provider:         "vpngate",
			ProviderIdentity: identity,
			ProviderNodeID:   get(row, "HostName"),
			Country:          get(row, "CountryLong"),
			CountryCode:      countryCode,
			HostName:         get(row, "HostName"),
			IPAddress:        ipAddress,
			RemoteHost:       remoteHost,
			RemotePort:       remotePort,
			Transport:        transport,
			SourceScore:      safeInt(get(row, "Score")),
			SourcePingMS:     safeInt(get(row, "Ping")),
			SourceSpeedBPS:   int64(safeInt(get(row, "Speed"))),
			SourceSessions:   safeInt(get(row, "NumVpnSessions")),
			ConfigText:       configText,
			FetchedAt:        now,
		})
		seen[identity] = true
		result.Stats.ValidRows++
	}
	// If every data record was syntactically unreadable, preserve the previous
	// all-or-nothing error for callers rather than reporting a successful empty
	// snapshot.  Once at least one record was parsed, individual bad rows are
	// intentionally tolerated and the usable nodes are returned.
	if parsedRows == 0 && firstCSVErr != nil {
		return ParseResult{}, fmt.Errorf("VPNGate CSV row unreadable: %w", firstCSVErr)
	}
	return result, nil
}

// parseRecord parses one VPNGate data line.  The strict pass catches truly
// broken quoted fields.  If it fails, retry with LazyQuotes so that a bare
// quote in an otherwise unquoted field (which VPNGate occasionally publishes)
// does not discard the entire response.  The retry is accepted only when the
// fields required to identify a node are present; this keeps truncated rows
// malformed while allowing harmless metadata corruption.
func parseRecord(line string, required []int) ([]string, error) {
	parse := func(lazy bool) ([]string, error) {
		reader := csv.NewReader(strings.NewReader(line))
		reader.FieldsPerRecord = -1
		reader.LazyQuotes = lazy
		return reader.Read()
	}

	row, err := parse(false)
	if err == nil {
		return row, nil
	}
	lazyRow, lazyErr := parse(true)
	if lazyErr == nil {
		complete := true
		for _, idx := range required {
			if idx >= len(lazyRow) {
				complete = false
				break
			}
		}
		if complete {
			return lazyRow, nil
		}
	}
	return nil, err
}
