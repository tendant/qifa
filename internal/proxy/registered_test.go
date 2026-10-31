package proxy

import "testing"

// kamal-proxy colours its table even when piped, so the escapes are part of
// what has to be parsed. This is real output from a live proxy.
const listOutput = "\x1b[3;94mService\x1b[0m             \x1b[3;94mHost\x1b[0m                     \x1b[3;94mPath\x1b[0m  \x1b[3;94mTarget\x1b[0m                                    \x1b[3;94mState\x1b[0m    \x1b[3;94mTLS\x1b[0m  \n" +
	"\x1b[1;34mlogging\x1b[0m             \x1b[mlogs.fulcruminfo.cn\x1b[0m      \x1b[m/\x1b[0m     \x1b[mlogging-web-efd05bb90de9:8427\x1b[0m             \x1b[mrunning\x1b[0m  \x1b[mno\x1b[0m   \n" +
	"\x1b[1;34mxiaoxiang-liver\x1b[0m     \x1b[mmp.liver.fulcruminfo.cn\x1b[0m  \x1b[m/\x1b[0m     \x1b[mxiaoxiang-liver-web-bb583740ded9:8090\x1b[0m     \x1b[mrunning\x1b[0m  \x1b[mno\x1b[0m   \n"

func TestParseRegistrations(t *testing.T) {
	rows := parseRegistrations(listOutput)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(rows), rows)
	}
	got, ok := rows["xiaoxiang-liver"]
	if !ok {
		t.Fatal("xiaoxiang-liver missing")
	}
	if got.Host != "mp.liver.fulcruminfo.cn" {
		t.Errorf("host = %q", got.Host)
	}
	if got.Target != "xiaoxiang-liver-web-bb583740ded9:8090" {
		t.Errorf("target = %q", got.Target)
	}
	if got.State != "running" {
		t.Errorf("state = %q", got.State)
	}
	// The header row must not be mistaken for a service.
	if _, ok := rows["Service"]; ok {
		t.Error("header parsed as a registration")
	}
}

// Real output of kamal-proxy v0.10.2 (the default) with two routes, one a
// wildcard. The table gained a Rollout column; the JSON is what reconcile
// reads first.
const listOutputV010 = "\u001b[3;94mService\u001b[0m  \u001b[3;94mHost\u001b[0m             \u001b[3;94mPath\u001b[0m  \u001b[3;94mTarget\u001b[0m        \u001b[3;94mState\u001b[0m    \u001b[3;94mTLS\u001b[0m  \u001b[3;94mRollout\u001b[0m  \n\u001b[1;34mweb-app\u001b[0m  \u001b[mapp.example.com\u001b[0m  \u001b[m/\u001b[0m     \u001b[m127.0.0.1:9\u001b[0m   \u001b[mrunning\u001b[0m  \u001b[mno\u001b[0m   \u001b[mno\u001b[0m       \n\u001b[1;34mwild\u001b[0m     \u001b[m*\u001b[0m                \u001b[m/\u001b[0m     \u001b[m127.0.0.1:10\u001b[0m  \u001b[mrunning\u001b[0m  \u001b[mno\u001b[0m   \u001b[mno\u001b[0m       \n"

const listJSONV010 = `{
  "web-app": {
    "hosts": [
      "app.example.com"
    ],
    "path_prefixes": [
      "/"
    ],
    "tls": false,
    "targets": [
      "127.0.0.1:9"
    ],
    "read_targets": [],
    "state": "running",
    "rollout": {
      "enabled": false,
      "percentage": 0,
      "allowlist": [],
      "targets": [],
      "read_targets": []
    }
  },
  "wild": {
    "hosts": [
      "*"
    ],
    "path_prefixes": [
      "/"
    ],
    "tls": false,
    "targets": [
      "127.0.0.1:10"
    ],
    "read_targets": [],
    "state": "running",
    "rollout": {
      "enabled": false,
      "percentage": 0,
      "allowlist": [],
      "targets": [],
      "read_targets": []
    }
  }
}
`

func TestParseRegistrationsV010(t *testing.T) {
	for name, rows := range map[string]map[string]Registration{
		"table": parseRegistrations(listOutputV010),
		"json":  mustJSON(t, listJSONV010),
	} {
		if len(rows) != 2 {
			t.Fatalf("%s: want 2 rows, got %+v", name, rows)
		}
		if got := rows["web-app"]; got.Host != "app.example.com" || got.Target != "127.0.0.1:9" || got.State != "running" {
			t.Errorf("%s: web-app = %+v", name, got)
		}
		if got := rows["wild"]; got.Host != "*" || got.Target != "127.0.0.1:10" || got.State != "running" {
			t.Errorf("%s: wildcard = %+v", name, got)
		}
	}
	if _, err := parseRegistrationsJSON(listOutputV010); err == nil {
		t.Error("table accepted as JSON")
	}
}

func mustJSON(t *testing.T, s string) map[string]Registration {
	t.Helper()
	rows, err := parseRegistrationsJSON(s)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
