package contextmode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// FetchTextFromURL downloads an HTML page and returns a structured
// markdown-ish text blob plus a title. Heavy stripping: <script>,
// <style>, <nav>, <header>, <footer>, <aside>, <noscript>, <form>,
// <iframe>, <svg> are all dropped before walking.
func FetchTextFromURL(ctx context.Context, url string, maxBytes int) (title, body string, err error) {
	if maxBytes <= 0 {
		maxBytes = 5 * 1024 * 1024
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	// A real-looking UA dodges the most common 403 / cloudflare
	// challenges; this is a single-user tool, not a crawler.
	req.Header.Set("User-Agent", "gogents-contextmode/0.1 (+https://github.com/thearray/gogents)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")

	cl := &http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}

	body0, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)))
	if err != nil {
		return "", "", err
	}
	if len(body0) == 0 {
		return "", "", errors.New("empty response body")
	}

	doc, err := html.Parse(strings.NewReader(string(body0)))
	if err != nil {
		return "", "", fmt.Errorf("parse html: %w", err)
	}
	title = extractTitle(doc)
	body = extractText(doc)
	return title, body, nil
}

func extractTitle(n *html.Node) string {
	var found string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != "" {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Title {
			if c := n.FirstChild; c != nil && c.Type == html.TextNode {
				found = strings.TrimSpace(c.Data)
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return found
}

// dropAtoms is the set of element types we skip entirely during
// extraction. They never carry the "main content" we want to index.
var dropAtoms = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Nav: true,
	atom.Header: true, atom.Footer: true, atom.Aside: true,
	atom.Noscript: true, atom.Form: true, atom.Iframe: true,
	atom.Svg: true, atom.Button: true,
}

// blockAtoms break paragraphs — emit a blank line between blocks so
// the chunker can split on heading boundaries cleanly.
var blockAtoms = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true,
	atom.Li: true, atom.Br: true, atom.Hr: true, atom.Tr: true, atom.Td: true,
	atom.Th: true, atom.Pre: true, atom.Blockquote: true,
}

// headingAtom returns the markdown prefix for a heading element ("##",
// "###" etc) so the chunker's strong-heading split picks them up.
func headingAtom(a atom.Atom) string {
	switch a {
	case atom.H1:
		return "# "
	case atom.H2, atom.H3:
		return "## "
	case atom.H4, atom.H5, atom.H6:
		return "### "
	}
	return ""
}

// extractText walks the DOM emitting plain text plus light markdown
// markers for headings, code, and lists. It's not a full md generator,
// just enough structure for the chunker and BM25 to do their work.
func extractText(root *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, inPre bool) {
		if n == nil {
			return
		}
		if n.Type == html.TextNode {
			t := n.Data
			if !inPre {
				t = collapseWS(t)
			}
			if t != "" {
				sb.WriteString(t)
			}
			return
		}
		if n.Type != html.ElementNode {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, inPre)
			}
			return
		}
		if dropAtoms[n.DataAtom] {
			return
		}
		switch n.DataAtom {
		case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
			sb.WriteString("\n\n")
			sb.WriteString(headingAtom(n.DataAtom))
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, false)
			}
			sb.WriteString("\n\n")
			return
		case atom.A:
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, inPre)
			}
			return
		case atom.Code:
			sb.WriteString("`")
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, inPre)
			}
			sb.WriteString("`")
			return
		case atom.Pre:
			sb.WriteString("\n\n```\n")
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, true)
			}
			sb.WriteString("\n```\n\n")
			return
		case atom.Li:
			sb.WriteString("\n- ")
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, inPre)
			}
			return
		case atom.Br:
			sb.WriteString("\n")
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inPre)
		}
		if blockAtoms[n.DataAtom] {
			sb.WriteString("\n\n")
		}
	}
	walk(root, false)
	return tidy(sb.String())
}

// collapseWS replaces runs of whitespace with a single space, the way
// HTML rendering normally does (when not in <pre>).
func collapseWS(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	prev := byte(' ')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' || c == '\t' || c == '\r' {
			c = ' '
		}
		if c == ' ' && prev == ' ' {
			continue
		}
		b.WriteByte(c)
		prev = c
	}
	return b.String()
}

// tidy collapses 3+ consecutive blank lines to a single blank line so
// the chunker has clean section boundaries instead of giant gaps.
func tidy(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}
