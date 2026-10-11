package fakeoidctest

import (
	"bytes"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// Account is one choice of the page /authorize shows when it has no login_hint.
type Account struct {
	// Email is the value of the account's button.
	Email string
	// Note is the warning shown with the account, empty for a Workspace account.
	Note string
}

// ChooserAccounts reads the accounts off the chooser page, in the order shown.
// It fails the test for a page that has none or whose forms do not use GET.
func ChooserAccounts(t testing.TB, page []byte) []Account {
	t.Helper()
	var out []Account
	for _, f := range chooserForms(t, page) {
		out = append(out, Account{Email: f.email, Note: f.note})
	}
	if len(out) == 0 {
		t.Fatalf("fakeoidctest: the chooser page has no account button:\n%s", page)
	}
	return out
}

// PressAccount returns the URL a browser requests when the button of email on
// the chooser page, which was served from pageURL, is pressed: the form's action
// with its hidden fields and login_hint=email.
func PressAccount(t testing.TB, pageURL *url.URL, page []byte, email string) *url.URL {
	t.Helper()
	for _, f := range chooserForms(t, page) {
		if f.email != email {
			continue
		}
		action, err := pageURL.Parse(f.action)
		if err != nil {
			t.Fatalf("fakeoidctest: the chooser form action %q: %v", f.action, err)
		}
		q := url.Values{}
		for k, v := range f.hidden {
			q[k] = v
		}
		q.Set("login_hint", email)
		action.RawQuery = q.Encode()
		return action
	}
	t.Fatalf("fakeoidctest: the chooser page has no button for %s:\n%s", email, page)
	return nil
}

type chooserForm struct {
	action string
	hidden url.Values
	email  string
	note   string
}

func chooserForms(t testing.TB, page []byte) []chooserForm {
	t.Helper()
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		t.Fatalf("fakeoidctest: the chooser page is not HTML: %v", err)
	}
	var forms []chooserForm
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "form" {
			if f, ok := readForm(t, n); ok {
				forms = append(forms, f)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return forms
}

func readForm(t testing.TB, form *html.Node) (chooserForm, bool) {
	t.Helper()
	f := chooserForm{action: attr(form, "action"), hidden: url.Values{}}
	if m := attr(form, "method"); !strings.EqualFold(m, "get") {
		t.Fatalf("fakeoidctest: a chooser form uses method %q, want get", m)
	}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch {
			case n.Data == "input" && attr(n, "type") == "hidden":
				f.hidden.Add(attr(n, "name"), attr(n, "value"))
			case n.Data == "button" && attr(n, "name") == "login_hint":
				f.email = attr(n, "value")
			case attr(n, "class") == "note":
				f.note = strings.TrimSpace(text(n))
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(form)
	return f, f.email != ""
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func text(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(text(c))
	}
	return b.String()
}
