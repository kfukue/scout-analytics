package main

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// urlMask replaces a URL removed from an error text.
const urlMask = "<url>"

// urlInTextRe matches a URL in an error text: a scheme, "://", then
// everything up to the next space, quote, angle bracket or backtick, less
// any punctuation at its end ("dial wss://host/key: bad handshake" keeps its
// colon). A *url.Error prints as `Post "https://host/key": dial tcp ...`, so
// the quote ends the match and the rest of the error is kept.
var urlInTextRe = regexp.MustCompile("[A-Za-z][A-Za-z0-9+.\\-]*://[^\\s\"'<>`]*[^\\s\"'<>`.,:;)\\]]")

// minKnownPathLen is the shortest path of a known URL that redactURLs
// replaces on its own: shorter ones ("/", "/rpc", "/v1") are generic words
// that would be blanked everywhere, and too short to hold a key.
const minKnownPathLen = 8

// redactURLs returns s with each of the known URLs (the configured node URLs)
// and every other scheme://... URL in it replaced by urlMask. A node URL can
// carry an API key in its path or query, so error texts are passed through
// this before they are stored or logged. The path and query of a known URL
// are replaced too (see knownURLParts): a proxy page can echo only the
// request path ("Cannot POST /v2/<key>").
func redactURLs(s string, known ...string) string {
	for _, p := range knownURLParts(known) {
		s = strings.ReplaceAll(s, p, urlMask)
	}
	return urlInTextRe.ReplaceAllString(s, urlMask)
}

// knownURLParts returns the texts redactURLs replaces for the known URLs,
// longest first (so a whole URL goes before its path): each non-empty URL
// as given, and when it parses, its path and path?query as sent in a
// request (paths of at least minKnownPathLen bytes only) and its raw query.
func knownURLParts(known []string) []string {
	var parts []string
	add := func(p string) {
		if p != "" && !slices.Contains(parts, p) {
			parts = append(parts, p)
		}
	}
	for _, raw := range known {
		add(raw)
		u, err := url.Parse(raw)
		if raw == "" || err != nil {
			continue
		}
		if p := u.EscapedPath(); len(p) >= minKnownPathLen {
			add(p)
			if u.RawQuery != "" {
				add(p + "?" + u.RawQuery)
			}
		}
		add(u.RawQuery)
	}
	slices.SortStableFunc(parts, func(a, b string) int { return len(b) - len(a) })
	return parts
}

// redactedError is an error whose text had its URLs removed. Unwrap gives the
// original error, so errors.Is / errors.As (time-outs, JSON-RPC errors) still
// classify it as before.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redactErr returns err with redactURLs applied to its text (err itself when
// the text has no URL in it, nil for nil).
func redactErr(err error, known ...string) error {
	if err == nil {
		return nil
	}
	msg := redactURLs(err.Error(), known...)
	if msg == err.Error() {
		return err
	}
	return &redactedError{msg: msg, err: err}
}

// urlOrigin returns only the scheme and host of a URL ("https://host:port"),
// for start-up lines and messages that name a node: its path, query, user
// info and fragment can hold an API key. A value that does not parse as
// scheme://host gives urlMask.
func urlOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return urlMask
	}
	return u.Scheme + "://" + u.Host
}
