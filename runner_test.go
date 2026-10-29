package main

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateURL(t *testing.T) {
	bad := []string{"", "--exec rm -rf ~", "-o/etc/passwd", "file:///etc/passwd", "ftp://x.com/a",
		"javascript:alert(1)", "https://", "https://a.com/\nb", "https://a.com/ b"}
	for _, u := range bad {
		if _, err := ValidateURL(u); err == nil {
			t.Errorf("accepted %q", u)
		}
	}
	for _, u := range []string{"https://www.youtube.com/watch?v=x", "http://127.0.0.1:8000/a.mp4"} {
		if _, err := ValidateURL(u); err != nil {
			t.Errorf("rejected %q: %v", u, err)
		}
	}
}

func TestArgsAlwaysEndWithSeparatorAndURL(t *testing.T) {
	link := "https://example.com/v"
	for _, preset := range []string{"best", "compat", "mp3", "audio"} {
		o := JobOptions{Preset: preset, Height: 720, Subs: true, Sponsor: true, Embed: true}
		o.normalize()
		a := ytdlpArgs(o, ytdlpPaths{Home: "/h", Temp: "/t"}, link)
		if a[len(a)-2] != "--" || a[len(a)-1] != link {
			t.Fatalf("%s: url not after --: %v", preset, a[len(a)-3:])
		}
		joined := strings.Join(a, " ")
		if strings.Contains(joined, "--exec") {
			t.Fatal("--exec must never be emitted")
		}
		if (preset == "mp3" || preset == "audio") && strings.Contains(joined, "--embed-subs") {
			t.Fatal("subs on audio preset")
		}
	}
	g := gallerydlArgs(JobOptions{}, gallerydlRun{Dir: "/d", URLs: []string{link}})
	if g[len(g)-2] != "--" || g[len(g)-1] != link {
		t.Fatal("gallery-dl url not after --")
	}
}

func TestParseProgress(t *testing.T) {
	p, ok := parseProgress("SGR:P|downloading|1048576|4194304|NA|524288.5|6|2|5|Title | with pipe")
	if !ok || p.Downloaded != 1<<20 || p.Total != 4<<20 || p.ETA != 6 || p.Index != 2 || p.N != 5 || p.Title != "Title | with pipe" {
		t.Fatalf("%+v", p)
	}
	p, _ = parseProgress("SGR:P|downloading|100|NA|400.0|NA|NA|NA|NA|x")
	if p.Total != 400 {
		t.Fatalf("estimate fallback: %+v", p)
	}
	if _, ok := parseProgress("[download] 10%"); ok {
		t.Fatal("parsed foreign line")
	}
}

func TestRouterHostLists(t *testing.T) {
	if !hostMatch("i.imgur.com", galleryFirstHosts) || hostMatch("notimgur.com", galleryFirstHosts) {
		t.Fatal("suffix match broken")
	}
	if !hostMatch("x.com", mixedHosts) || hostMatch("youtube.com", mixedHosts) {
		t.Fatal("mixed list broken")
	}
}

func TestCookieStore(t *testing.T) {
	c := NewCookieStore(t.TempDir())
	if _, err := c.Import([]byte(".instagram.com\tTRUE\t/\tTRUE\t0\tsessionid\tabc\n"), []string{"https://www.Instagram.com/"}); err != nil {
		t.Fatal(err)
	}
	if c.For("instagram.com") == "" || c.For("i.instagram.com") == "" || c.For("x.com") != "" {
		t.Fatal("cookie lookup")
	}
	if _, err := c.Import([]byte(".instagram.com\tTRUE\t/\tTRUE\t0\tsessionid\tabc\n"), []string{"../../etc"}); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestFallbackEngine(t *testing.T) {
	cases := []struct{ eng, msg, want string }{
		{EngYTDLP, "ERROR: [Instagram] DeHgyYjjK3o: No video formats found!; please report this issue", EngGalleryDL},
		{EngYTDLP, "ERROR: [twitter] 123: There is no video in this post", EngGalleryDL},
		{EngYTDLP, "ERROR: [youtube] x: Video unavailable. This video is private", ""},
		{EngYTDLP, "HTTP Error 404: Not Found", ""},
		{EngGalleryDL, "[gallery-dl][error] No suitable extractor found for 'https://x'", EngYTDLP},
		{EngGalleryDL, "[instagram][error] HttpError: '401 Unauthorized'", ""},
	}
	for _, c := range cases {
		if got := fallbackEngine(c.eng, c.msg); got != c.want {
			t.Errorf("%s %q → %q, want %q", c.eng, c.msg, got, c.want)
		}
	}
}

func TestLoginHint(t *testing.T) {
	err := withLoginHint("www.instagram.com", errors.New("[instagram][error] Redirect to login page"))
	if !strings.Contains(err.Error(), "instagram.com için cookies.txt") {
		t.Fatal(err)
	}
	if withLoginHint("x.com", errors.New("HTTP Error 404")).Error() != "HTTP Error 404" {
		t.Fatal("non-login error rewritten")
	}
}

func TestPickErrorPrefersGalleryLoginForImagePosts(t *testing.T) {
	yt := errors.New("ERROR: [Instagram] DeHgyYjjK3o: No video formats found!; please report this issue")
	gd := errors.New("[instagram][error] HTTP redirect to login page (https://www.instagram.com/accounts/login/)")
	got := withLoginHint("instagram.com", pickError(yt, gd))
	if !strings.Contains(got.Error(), "instagram.com için cookies.txt ekle") {
		t.Fatal(got)
	}
	// A real yt-dlp error (404) must win over gallery-dl "no extractor".
	yt2 := errors.New("HTTP Error 404: Not Found")
	if pickError(yt2, errors.New("No suitable extractor found")) != yt2 {
		t.Fatal("wrong error chosen")
	}
}

// X links name files "[tweet ID] [media ID]" in every layout; other sites keep "[id]".
func TestXTemplateKeepsTweetID(t *testing.T) {
	tmpl := func(o JobOptions, p ytdlpPaths, link string) string {
		a := ytdlpArgs(o, p, link)
		for i, s := range a {
			if s == "-o" {
				return a[i+1]
			}
		}
		t.Fatal("no -o")
		return ""
	}
	layouts := []struct {
		o JobOptions
		p ytdlpPaths
	}{
		{JobOptions{}, ytdlpPaths{}},
		{JobOptions{Playlist: true}, ytdlpPaths{}},
		{JobOptions{}, ytdlpPaths{Flat: true}},
		{JobOptions{Playlist: true}, ytdlpPaths{Flat: true}},
		{JobOptions{Mode: "temp"}, ytdlpPaths{}},
		{JobOptions{Mode: "temp", Playlist: true}, ytdlpPaths{}},
	}
	for _, l := range layouts {
		for _, link := range []string{"https://x.com/a/status/719944021058060289", "https://mobile.twitter.com/a/status/1"} {
			if got := tmpl(l.o, l.p, link); !strings.Contains(got, "[%(display_id)s] [%(id)s].%(ext)s") || strings.Count(got, "%(id)s") != 1 {
				t.Errorf("%+v %s: %s", l, link, got)
			}
		}
		if got := tmpl(l.o, l.p, "https://www.youtube.com/watch?v=jNQXAC9IVRw"); strings.Contains(got, "display_id") || !strings.HasSuffix(got, " [%(id)s].%(ext)s") {
			t.Errorf("youtube %+v: %s", l, got)
		}
	}
	// The planned path is printed before yt-dlp creates its folder.
	if a := strings.Join(ytdlpArgs(JobOptions{}, ytdlpPaths{}, "https://a.b/c"), " "); !strings.Contains(a, "--print pre_process:"+dirPrefix+"%(filename)s") {
		t.Fatalf("no pre_process print: %s", a)
	}
}

// Real yt-dlp messages: errors that cannot change on a retry fail at once.
func TestTransientClassification(t *testing.T) {
	cases := []struct {
		msg       string
		transient bool
	}{
		{"ERROR: [youtube] aaaaaaaaaaa: This video is unavailable", false},
		{"ERROR: [youtube] aaaaaaaaaaa: Video unavailable. This video has been removed by the uploader", false},
		{"ERROR: [youtube:truncated_id] jNQXAC9IVR: Incomplete YouTube ID jNQXAC9IVR. URL https://www.youtube.com/watch?v=jNQXAC9IVR looks truncated.", false},
		{"ERROR: [youtube] xxxxxxxxxxx: This video is no longer available because the YouTube account associated with this video has been terminated.", false},
		{"ERROR: [youtube] xxxxxxxxxxx: Join this channel to get access to members-only content like this video, and other exclusive perks.", false},
		{"ERROR: [youtube:tab] UCxxxx: The channel/playlist does not exist and the URL redirected to youtube.com home page", false},
		{"ERROR: [TikTok] 1: This video does not exist.", false},
		{"ERROR: [twitter] 1: Requested tweet is unavailable", false},
		{"ERROR: [generic] Unsupported URL: https://example.com/", false},
		{"ERROR: [Instagram] x: The webpage request was redirected to the login page. You have exceeded the rate-limit for accessing posts anonymously", false},
		// gallery-dl (exception.py, job.py: "<class>: <message>"), as İndirgen reports it
		{"[gallery-dl] NotFoundError: Requested post could not be found", false},
		{"[instagram][error] NotFoundError: Requested user could not be found", false},
		{"[gallery-dl] AuthorizationError: Insufficient privileges to access this resource", false},
		{"[twitter][error] AuthRequired: 'twid' cookie needed to access this resource", false},
		{"[twitter][error] AuthRequired: Protected Tweet", false},
		{"[deviantart][error] AuthRequired: Account credentials required", false},
		{"[gallery-dl] AuthenticationError: Invalid login credentials", false},
		{"[gallery-dl][error] Unsupported URL 'https://example.com/'", false},
		{"[instagram][error] HttpError: '429 Too Many Requests' for 'https://www.instagram.com/api/v1/users/web_profile_info/'", true},
		// Network trouble and throttling: worth another try.
		{"ERROR: [youtube] dQw4w9WgXcQ: Unable to download API page: ('Unable to connect to proxy', NewConnectionError(...))", true},
		{"ERROR: [youtube] dQw4w9WgXcQ: Unable to download webpage: HTTP Error 503: Service Unavailable (caused by <HTTPError 503: Service Unavailable>)", true},
		{"ERROR: [download] Got error: The read operation timed out", true},
		{"ERROR: [youtube] x: Unable to download webpage: HTTP Error 429: Too Many Requests", true},
	}
	for _, c := range cases {
		if got := transient(c.msg); got != c.transient {
			t.Errorf("transient(%q) = %v, want %v", c.msg, got, c.transient)
		}
	}
}
