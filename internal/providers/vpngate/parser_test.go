package vpngate

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/masteralanlab/free-proxy/internal/domain"
)

func TestParseResponse(t *testing.T) {
	cfg := "client\ndev tun\nproto udp\nremote 1.2.3.4 1194 udp\n"
	enc := base64.StdEncoding.EncodeToString([]byte(cfg))
	header := "#HostName,IP,Score,Ping,Speed,CountryLong,CountryShort,NumVpnSessions,OpenVPN_ConfigData_Base64"
	row := "host1,1.2.3.4,100,50,1000000,Japan,JP,5," + enc
	dup := "host1b,1.2.3.4,90,60,900000,Japan,JP,3," + enc
	text := strings.Join([]string{"*vpn servers", header, row, dup, "*"}, "\n")

	res, err := ParseResponse(text, 300, time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(res.Nodes))
	}
	if res.Stats.TotalRows != 2 || res.Stats.ValidRows != 1 || res.Stats.DuplicateRows != 1 {
		t.Fatalf("stats = %+v", res.Stats)
	}
	n := res.Nodes[0]
	if n.IPAddress != "1.2.3.4" || n.RemotePort != 1194 || n.Transport != domain.TransportUDP {
		t.Fatalf("node remote = %s:%d/%s", n.RemoteHost, n.RemotePort, n.Transport)
	}
	if n.CountryCode != "JP" || n.Country != "Japan" {
		t.Fatalf("country = %s/%s", n.CountryCode, n.Country)
	}
	if n.SourceScore != 100 || n.SourceSessions != 5 {
		t.Fatalf("source score/sessions = %d/%d", n.SourceScore, n.SourceSessions)
	}
	if !strings.HasPrefix(n.ID, "jp-") {
		t.Fatalf("id = %s, want jp- prefix", n.ID)
	}
	if n.Provider != "vpngate" || n.ProviderIdentity != "vpngate:1.2.3.4" {
		t.Fatalf("identity = %s", n.ProviderIdentity)
	}
}

func TestParseResponseRejectsMissingHeader(t *testing.T) {
	if _, err := ParseResponse("just,some,data\n1,2,3", 10, time.Now()); err == nil {
		t.Fatal("expected error for missing header")
	}
}

func TestParseResponseLimit(t *testing.T) {
	cfg := "remote 9.9.9.9 443 tcp\n"
	enc := base64.StdEncoding.EncodeToString([]byte(cfg))
	var b strings.Builder
	b.WriteString("#IP,CountryShort,CountryLong,HostName,Score,Ping,Speed,NumVpnSessions,OpenVPN_ConfigData_Base64\n")
	for i := 0; i < 5; i++ {
		// distinct IPs so they are not deduplicated
		ip := "10.0.0." + string(rune('1'+i))
		b.WriteString(ip + ",US,United States,h,1,1,1,1," + enc + "\n")
	}
	res, err := ParseResponse(b.String(), 2, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2 (limited)", len(res.Nodes))
	}
	if res.Stats.TotalRows != 5 {
		t.Fatalf("total rows = %d, want 5", res.Stats.TotalRows)
	}
}

func TestParseResponseRejectsTruncatedCSV(t *testing.T) {
	text := "#IP,CountryShort,OpenVPN_ConfigData_Base64\n\"1.2.3.4,JP,unterminated"
	if _, err := ParseResponse(text, 60, time.Now()); err == nil {
		t.Fatal("expected malformed CSV to fail instead of accepting a partial snapshot")
	}
}

func TestParseResponseToleratesBareQuotes(t *testing.T) {
	cfg := "remote 1.2.3.4 1194 udp\n"
	enc := base64.StdEncoding.EncodeToString([]byte(cfg))
	header := "#HostName,IP,Comment,OpenVPN_ConfigData_Base64"
	// The comment contains a bare quote, which is common in the upstream feed
	// even though it is not escaped as a CSV quoted field.
	row := "host1,1.2.3.4,operator says \"hello\"," + enc

	res, err := ParseResponse(strings.Join([]string{header, row}, "\n"), 10, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1 (bare quote should not abort the response)", len(res.Nodes))
	}
	if res.Stats.MalformedRows != 0 {
		t.Fatalf("malformed rows = %d, want 0", res.Stats.MalformedRows)
	}
}

func TestParseResponseSkipsMalformedRows(t *testing.T) {
	cfg := "remote 5.6.7.8 443 tcp\n"
	enc := base64.StdEncoding.EncodeToString([]byte(cfg))
	header := "#HostName,IP,OpenVPN_ConfigData_Base64"
	bad := `broken,5.6.7.8,"unterminated`
	good := "host2,5.6.7.9," + enc

	res, err := ParseResponse(strings.Join([]string{header, bad, good}, "\n"), 10, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Nodes) != 1 || res.Nodes[0].IPAddress != "5.6.7.9" {
		t.Fatalf("nodes = %+v, want only the valid row", res.Nodes)
	}
	if res.Stats.TotalRows != 2 || res.Stats.MalformedRows != 1 {
		t.Fatalf("stats = %+v, want total 2 and malformed 1", res.Stats)
	}
}
