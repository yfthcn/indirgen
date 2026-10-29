package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var future = time.Now().Add(300 * 24 * time.Hour).Unix()
var past = time.Now().Add(-48 * time.Hour).Unix()

func netscapeExport() string {
	f := func(v int64) string { return strconv.FormatInt(v, 10) }
	return "# Netscape HTTP Cookie File\n" +
		".instagram.com\tTRUE\t/\tTRUE\t" + f(future) + "\tsessionid\tIGSESSION\n" +
		".instagram.com\tTRUE\t/\tTRUE\t" + f(future) + "\tcsrftoken\tabc\n" +
		"#HttpOnly_.x.com\tTRUE\t/\tTRUE\t" + f(past) + "\tauth_token\tXAUTH\n" +
		".x.com\tTRUE\t/\tTRUE\t" + f(future) + "\tct0\tq\n" +
		"www.mybank.hu\tFALSE\t/\tTRUE\t" + f(future) + "\tBANKSESSION\tsecret\n" +
		".bbc.co.uk\tTRUE\t/\tFALSE\t0\tckns\t1\n" +
		"broken line without tabs\n"
}

func TestSiteOf(t *testing.T) {
	for in, want := range map[string]string{
		".www.instagram.com": "instagram.com", "i.instagram.com": "instagram.com", "x.com": "x.com",
		".bbc.co.uk": "bbc.co.uk", "www.e-devlet.gov.tr": "e-devlet.gov.tr", "music.youtube.com": "youtube.com",
	} {
		if got := siteOf(in); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
}

func TestAnalyzeGroupsSitesWithoutWriting(t *testing.T) {
	c := NewCookieStore(t.TempDir())
	a, err := c.Analyze([]byte(netscapeExport()))
	if err != nil {
		t.Fatal(err)
	}
	if a.Format != "netscape" || a.Total != 6 || len(a.Sites) != 4 {
		t.Fatalf("%+v", a)
	}
	// known sites first, then alphabetical
	if a.Sites[0].Site != "instagram.com" || a.Sites[1].Site != "x.com" || a.Sites[2].Site != "bbc.co.uk" || a.Sites[3].Site != "mybank.hu" {
		t.Fatalf("order: %v", a.Sites)
	}
	ig, x := a.Sites[0], a.Sites[1]
	if !ig.HasLogin || ig.Expired || ig.Expires != future || ig.Count != 2 || ig.Label != "Instagram" {
		t.Fatalf("instagram: %+v", ig)
	}
	if !x.HasLogin || !x.Expired {
		t.Fatalf("x login should be expired: %+v", x)
	}
	if len(c.List()) != 0 {
		t.Fatal("analyze must not write anything")
	}
}

func TestImportOnlySelectedSites(t *testing.T) {
	dir := t.TempDir()
	c := NewCookieStore(dir)
	saved, err := c.Import([]byte(netscapeExport()), []string{"instagram.com", "https://www.x.com/home"})
	if err != nil || len(saved) != 2 {
		t.Fatalf("%v %v", saved, err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.txt"))
	if len(files) != 2 {
		t.Fatalf("files: %v", files)
	}
	ig, _ := os.ReadFile(filepath.Join(dir, "instagram.com.txt"))
	if strings.Contains(string(ig), "BANKSESSION") || strings.Contains(string(ig), "XAUTH") || !strings.Contains(string(ig), "IGSESSION") {
		t.Fatalf("instagram file leaked other sites:\n%s", ig)
	}
	xf, _ := os.ReadFile(filepath.Join(dir, "x.com.txt"))
	if !strings.Contains(string(xf), "#HttpOnly_.x.com\tTRUE") {
		t.Fatalf("HttpOnly prefix lost:\n%s", xf)
	}
	if c.For("www.instagram.com") == "" || c.For("mybank.hu") != "" {
		t.Fatal("lookup")
	}
	if _, err := c.Import([]byte(netscapeExport()), []string{"tiktok.com"}); err == nil {
		t.Fatal("site absent from export accepted")
	}
	if _, err := c.Import([]byte(netscapeExport()), nil); err == nil {
		t.Fatal("empty selection accepted")
	}
	// Re-import replaces only the selected site.
	refresh := ".instagram.com\tTRUE\t/\tTRUE\t" + strconv.FormatInt(future, 10) + "\tsessionid\tNEW\n"
	if _, err := c.Import([]byte(refresh), []string{"instagram.com"}); err != nil {
		t.Fatal(err)
	}
	ig, _ = os.ReadFile(filepath.Join(dir, "instagram.com.txt"))
	xf2, _ := os.ReadFile(filepath.Join(dir, "x.com.txt"))
	if !strings.Contains(string(ig), "NEW") || strings.Contains(string(ig), "IGSESSION") || string(xf2) != string(xf) {
		t.Fatal("refresh touched the wrong files")
	}
	list := c.List()
	if len(list) != 2 || list[0].Site != "instagram.com" || list[0].Updated.IsZero() || !list[1].Expired {
		t.Fatalf("list: %+v", list)
	}
}

// Cookie-Editor's default "Export → JSON" format.
const cookieEditorJSON = `[
 {"domain":".instagram.com","expirationDate":` + "4102444800" + `,"hostOnly":false,"httpOnly":true,"name":"sessionid","path":"/","sameSite":"no_restriction","secure":true,"session":false,"storeId":"0","value":"IGJSON"},
 {"domain":"www.instagram.com","hostOnly":true,"httpOnly":false,"name":"ig_nrcb","path":"/","secure":true,"session":true,"value":"1"},
 {"domain":".youtube.com","expirationDate":4102444800.5,"hostOnly":false,"name":"LOGIN_INFO","path":"/","secure":true,"session":false,"value":"YT"}
]`

func TestImportCookieEditorJSON(t *testing.T) {
	dir := t.TempDir()
	c := NewCookieStore(dir)
	a, err := c.Analyze([]byte(cookieEditorJSON))
	if err != nil || a.Format != "json" || len(a.Sites) != 2 {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := c.Import([]byte(cookieEditorJSON), []string{"instagram.com"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "instagram.com.txt"))
	s := string(b)
	for _, want := range []string{
		"#HttpOnly_.instagram.com\tTRUE\t/\tTRUE\t4102444800\tsessionid\tIGJSON",
		"www.instagram.com\tFALSE\t/\tTRUE\t0\tig_nrcb\t1",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	// Parsed back by the same reader the engines' format follows.
	cs, _, err := parseCookies(b)
	if err != nil || len(cs) != 2 {
		t.Fatalf("round trip: %v %v", cs, err)
	}
}

func TestSessionLoginCookie(t *testing.T) {
	s := summarize("instagram.com", []cookie{
		{Domain: ".instagram.com", Name: "sessionid", Expires: 0},
		{Domain: ".instagram.com", Name: "csrftoken", Expires: future},
	}, time.Now())
	if !s.HasLogin || s.Expires != 0 || s.Expired {
		t.Fatalf("%+v", s)
	}
}

func TestRejectsGarbage(t *testing.T) {
	c := NewCookieStore(t.TempDir())
	for _, in := range []string{"", "hello world", `{"a":1}`, "[]"} {
		if _, err := c.Analyze([]byte(in)); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}
