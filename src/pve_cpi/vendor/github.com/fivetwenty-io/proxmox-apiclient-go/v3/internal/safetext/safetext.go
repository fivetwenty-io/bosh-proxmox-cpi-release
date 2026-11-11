// Package safetext keeps bytes a server chose from reaching an error message
// or a log line as raw control characters. A hostile or broken server can put
// a newline, an escape sequence, or a text direction override in a value the
// client then prints, which forges log lines or rewrites what a terminal shows.
package safetext

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxQuoted is the most bytes of a server's value that Quote keeps.
const MaxQuoted = 64

// ellipsis follows a quoted value that Quote cut short.
const ellipsis = "..."

// Quote renders value as a Go string literal, with every control character,
// text direction override, and invalid byte escaped. It keeps at most
// MaxQuoted bytes of value, cut on a character boundary, and follows the
// closing quote with an ellipsis when it cut anything, so the message stays
// short and still shows what the server sent.
func Quote(value string) string {
	cut := len(value)

	if cut > MaxQuoted {
		cut = MaxQuoted
		// Back up to the start of a character, so the cut does not split one.
		for cut > 0 && !utf8.RuneStart(value[cut]) {
			cut--
		}
	}

	quoted := strconv.Quote(value[:cut])
	if cut < len(value) {
		quoted += ellipsis
	}

	return quoted
}

// truncationMarker follows text that Capped cut short.
const truncationMarker = "...(truncated)"

// Error returns err, or when its text holds a control character, a text
// direction override, or invalid UTF-8, an error with the same chain whose
// text has those escaped. An error with clean text comes back as the same
// value.
func Error(err error) error {
	return wrapEscaped(err, 0)
}

// CappedError is Error with a limit. When the escaped text of err is longer
// than limit bytes, the returned error carries the part that fits, cut on a
// character boundary, followed by a truncation marker. A newline and a tab are
// escaped too, so the text stays on one line.
func CappedError(err error, limit int) error {
	return wrapEscaped(err, limit)
}

func wrapEscaped(err error, limit int) error {
	if err == nil {
		return nil
	}

	text := err.Error()

	escaped := Capped(text, limit)
	if escaped == text {
		return err
	}

	return &escapedError{text: escaped, cause: err}
}

// escapedError is an error whose text is the escaped text of the error it
// wraps. errors.Is and errors.As see the wrapped error.
type escapedError struct {
	text  string
	cause error
}

// Error returns the escaped text.
func (e *escapedError) Error() string { return e.text }

// Unwrap returns the error whose text was escaped.
func (e *escapedError) Unwrap() error { return e.cause }

// GoString keeps the %#v form to the escaped text, because the default form
// would print the wrapped error and with it the raw bytes.
func (e *escapedError) GoString() string { return "safetext.Error(" + strconv.Quote(e.text) + ")" }

// unsafeRune reports whether r must not reach a log line as itself. It covers
// the C0 and C1 controls, DEL, the format characters (which hold the text
// direction overrides, isolates, and marks such as U+202A to U+202E, U+2066 to
// U+2069, U+200E, U+200F, and U+061C), and the line and paragraph separators.
func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029'
}

// needsEscape reports whether text holds a rune that escape would change.
func needsEscape(text string, keepLayout bool) bool {
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if mustEscape(r, size, keepLayout) {
			return true
		}

		i += size
	}

	return false
}

// mustEscape reports whether the rune r, decoded with the given size, is
// replaced by an escape sequence. A newline and a tab stay when keepLayout is
// set.
func mustEscape(r rune, size int, keepLayout bool) bool {
	if keepLayout && (r == '\n' || r == '\t') {
		return false
	}

	return (r == utf8.RuneError && size == 1) || unsafeRune(r)
}

// Multiline escapes text for an error message that may legitimately span
// lines. Every control character other than newline and tab is replaced by
// its escape sequence, and so are the text direction overrides and other
// format characters, the line and paragraph separators, and invalid UTF-8.
// Newline and tab stay, so a multi-line message from the server stays
// readable. A caller that prints the text on one line should use Line.
func Multiline(text string) string {
	return escape(text, true)
}

// Line escapes text like Multiline and also escapes newline and tab, so the
// result holds nothing that can start a second log line.
func Line(text string) string {
	return escape(text, false)
}

// Capped returns text escaped as Line does, cut so the escaped text is at most
// limit bytes. The cut falls between characters, and when it cut anything the
// result ends with a truncation marker, which is not counted in limit. A limit
// of zero or less keeps all of text.
func Capped(text string, limit int) string {
	return cappedEscape(text, limit, false)
}

// CappedMultiline returns text escaped as Multiline does, cut so the escaped
// text is at most limit bytes, with a truncation marker when it cut anything.
func CappedMultiline(text string, limit int) string {
	return cappedEscape(text, limit, true)
}

// CappedMultilineBytes is CappedMultiline for a body held as bytes. It never
// copies more than limit plus one character of body, so a very large body
// costs no more than a small one. The limit must be positive.
func CappedMultilineBytes(body []byte, limit int) string {
	if len(body) > limit+utf8.UTFMax {
		body = body[:limit+utf8.UTFMax]
	}

	return cappedEscape(string(body), limit, true)
}

func cappedEscape(text string, limit int, keepLayout bool) string {
	if limit <= 0 {
		return escape(text, keepLayout)
	}

	var out strings.Builder

	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		piece := text[i : i+size]

		if mustEscape(r, size, keepLayout) {
			piece = escapeText(piece)
		}

		if out.Len()+len(piece) > limit {
			out.WriteString(truncationMarker)

			break
		}

		out.WriteString(piece)

		i += size
	}

	return out.String()
}

// escape replaces each unsafe rune and each invalid byte in text with the
// escape sequence strconv.Quote would use for it.
func escape(text string, keepLayout bool) string {
	if !needsEscape(text, keepLayout) {
		return text
	}

	var out strings.Builder

	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])

		if mustEscape(r, size, keepLayout) {
			out.WriteString(escapeText(text[i : i+size]))
		} else {
			out.WriteString(text[i : i+size])
		}

		i += size
	}

	return out.String()
}

// escapeText quotes one character and removes the quote marks around it.
func escapeText(char string) string {
	quoted := strconv.Quote(char)

	return quoted[1 : len(quoted)-1]
}
