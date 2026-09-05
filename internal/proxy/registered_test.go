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
