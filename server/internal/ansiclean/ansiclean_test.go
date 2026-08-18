package ansiclean

import "testing"

func TestStrip_NoEscapes(t *testing.T) {
	cases := []string{
		"",
		"plain",
		"claude-sonnet-4-5",
		"MiniMax-M3[1m]", // legit Claude variant — survives.
		"MiniMax-M3[1m",  // even without the closing bracket.
		"some \n multi\n line text",
		"中文 \U0001F600 emoji",
	}
	for _, s := range cases {
		if got := Strip(s); got != s {
			t.Errorf("Strip(%q) = %q, want unchanged", s, got)
		}
	}
}

func TestStrip_CSI_Bold(t *testing.T) {
	got := Strip("\x1b[1mMiniMax-M3\x1b[0m")
	if got != "MiniMax-M3" {
		t.Fatalf("got %q", got)
	}
}

func TestStrip_CSI_Color(t *testing.T) {
	got := Strip("\x1b[31mFAIL\x1b[0m: bad input")
	if got != "FAIL: bad input" {
		t.Fatalf("got %q", got)
	}
}

func TestStrip_CSI_Param(t *testing.T) {
	// 256-colour foreground: ESC[38;2;R;G;Bm.
	got := Strip("\x1b[38;2;128;64;255mcolour\x1b[0m")
	if got != "colour" {
		t.Fatalf("got %q", got)
	}
}

func TestStrip_OSC_Title(t *testing.T) {
	// ESC]0;title\x07 -- typical xterm window-title OSC.
	got := Strip("\x1b]0;my title\x07body")
	if got != "body" {
		t.Fatalf("got %q", got)
	}
}

func TestStrip_OSC_WithST(t *testing.T) {
	// OSC terminated by ST (ESC \\) instead of BEL.
	got := Strip("\x1b]0;title\x1b\\body")
	if got != "body" {
		t.Fatalf("got %q", got)
	}
}

func TestStrip_PreservesContent(t *testing.T) {
	in := "the agent wrote: \x1b[33mwarning\x1b[0m — please review"
	want := "the agent wrote: warning — please review"
	if got := Strip(in); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestStrip_CursorClear(t *testing.T) {
	// ESC[2K -- erase line. Real-world from terminal-aware logs.
	got := Strip("above\x1b[2Kbelow")
	if got != "abovebelow" {
		t.Fatalf("got %q", got)
	}
}
