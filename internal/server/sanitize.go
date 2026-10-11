package server

import (
	"bytes"
	"encoding/json"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var urlAttrs = map[string]bool{"href": true, "src": true, "action": true, "formaction": true, "hx-get": true, "hx-post": true, "hx-put": true, "hx-patch": true, "hx-delete": true, "data-modal": true, "xlink:href": true}

var droppedAttrs = map[string]bool{"hx-headers": true, "hx-vars": true, "hx-ext": true, "hx-request": true, "hx-boost": true, "hx-history-elt": true, "srcdoc": true}

func allowedURL(v, base string) bool {
	v = strings.TrimSpace(v)
	return v == "" || strings.HasPrefix(v, "#") || strings.HasPrefix(v, "/static/") || v == base || strings.HasPrefix(v, base+"/") || strings.HasPrefix(v, base+"?")
}

func safeAttr(a html.Attribute, base string) bool {
	k := strings.ToLower(a.Key)
	v := strings.TrimSpace(a.Val)
	lv := strings.ToLower(v)
	switch {
	case strings.HasPrefix(k, "on"), strings.HasPrefix(k, "hx-on"), strings.HasPrefix(k, "data-hx-"), droppedAttrs[k]:
		return false
	case urlAttrs[k]:
		return allowedURL(v, base)
	case k == "hx-trigger":
		return !strings.ContainsAny(v, "[]")
	case k == "hx-vals":
		return !strings.HasPrefix(lv, "js:") && !strings.HasPrefix(lv, "javascript:")
	case k == "data-fill":
		var m map[string]string
		if json.Unmarshal([]byte(v), &m) != nil {
			return false
		}
		return m["action"] == "" || allowedURL(m["action"], base)
	}
	return !strings.Contains(lv, "javascript:")
}

func sanitizeHTML(body []byte, base string) []byte {
	ctx := &html.Node{Type: html.ElementNode, DataAtom: atom.Body, Data: "body"}
	nodes, err := html.ParseFragment(bytes.NewReader(body), ctx)
	if err != nil {
		return nil
	}
	var clean func(*html.Node) bool
	clean = func(n *html.Node) bool {
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Script:
				src := ""
				for _, a := range n.Attr {
					if a.Key == "src" {
						src = a.Val
					}
				}
				if !strings.HasPrefix(src, "/static/") || n.FirstChild != nil {
					return false
				}
			case atom.Iframe, atom.Object, atom.Embed, atom.Base, atom.Meta, atom.Style, atom.Frame, atom.Frameset:
				return false
			case atom.Link:
				ok := false
				for _, a := range n.Attr {
					ok = ok || a.Key == "href" && strings.HasPrefix(a.Val, "/static/")
				}
				if !ok {
					return false
				}
			}
			attrs := n.Attr[:0]
			for _, a := range n.Attr {
				if safeAttr(a, base) {
					attrs = append(attrs, a)
				}
			}
			n.Attr = attrs
		}
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			if !clean(c) {
				n.RemoveChild(c)
			}
			c = next
		}
		return true
	}
	var out bytes.Buffer
	for _, n := range nodes {
		if clean(n) {
			html.Render(&out, n)
		}
	}
	return out.Bytes()
}

func safeRedirect(v, node string) bool {
	return strings.HasPrefix(v, "/hosts/"+node+"/") || strings.HasPrefix(v, "/nodes/"+node+"/")
}
