package matcher

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

// MaxHTMLBodySize is the upper limit on HTML body size that will be canonicalized.
// Bodies larger than this are compared as raw strings to avoid OOM in CI.
const MaxHTMLBodySize = 1 << 20 // 1 MB

type HTMLCompareConfig struct {
	StripTags      map[string]bool
	StripAttrRegex []*regexp.Regexp
}

// CanonicalizeHTML parses, cleans, normalizes, and deterministically serializes HTML content.
// Returns an error if the input exceeds MaxHTMLBodySize.
func CanonicalizeHTML(input string, config HTMLCompareConfig) (string, error) {
	if input == "" {
		return "", nil
	}

	// Fix 5: 1MB size guard — avoids OOM on large responses.
	if len(input) > MaxHTMLBodySize {
		return "", fmt.Errorf("HTML body exceeds maximum size for canonicalization (%d bytes)", MaxHTMLBodySize)
	}

	doc, err := html.Parse(strings.NewReader(input))
	if err != nil {
		return "", err
	}

	// Fix 1: No cloneNode — operate directly on the freshly parsed tree.
	// The tree is not shared, so mutation is safe.
	filterTree(doc, config)
	normalizeTree(doc)

	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return "", err
	}

	return buf.String(), nil
}

func filterTree(n *html.Node, config HTMLCompareConfig) {
	stack := []*html.Node{n}
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		// Collect surviving children in DOM order, then push in reverse
		// so LIFO stack pops them FirstChild-first.
		var children []*html.Node
		var next *html.Node
		for c := node.FirstChild; c != nil; c = next {
			next = c.NextSibling // capture before potential RemoveChild

			if c.Type == html.CommentNode {
				node.RemoveChild(c)
				continue
			}

			if c.Type == html.ElementNode && config.StripTags[c.Data] {
				node.RemoveChild(c)
				continue
			}

			if c.Type == html.ElementNode {
				c.Attr = filterAttributes(c.Attr, config.StripAttrRegex)
			}

			children = append(children, c)
		}
		// Push in reverse so stack pops in DOM order.
		for i := len(children) - 1; i >= 0; i-- {
			stack = append(stack, children[i])
		}
	}
}

func filterAttributes(attrs []html.Attribute, regexes []*regexp.Regexp) []html.Attribute {
	if len(attrs) == 0 {
		return attrs
	}

	var filtered []html.Attribute
	for _, attr := range attrs {
		skip := false
		for _, re := range regexes {
			if re.MatchString(attr.Key) {
				skip = true
				break
			}
		}
		if !skip {
			filtered = append(filtered, attr)
		}
	}
	return filtered
}

type stackItem struct {
	node     *html.Node
	preserve bool
}

func normalizeTree(n *html.Node) {
	stack := []stackItem{{node: n, preserve: false}}
	for len(stack) > 0 {
		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		node := item.node

		// Collect surviving children with their preserve state in DOM order,
		// then push in reverse so LIFO stack pops them FirstChild-first.
		var children []stackItem
		var next *html.Node
		for c := node.FirstChild; c != nil; c = next {
			next = c.NextSibling // capture before potential RemoveChild

			childPreserve := item.preserve
			if c.Type == html.ElementNode {
				// Only <pre> and <textarea> preserve whitespace per the HTML spec.
				// <code> is an inline element with no whitespace-preservation semantics.
				switch c.Data {
				case "pre", "textarea":
					childPreserve = true
				}

				sort.Slice(c.Attr, func(i, j int) bool {
					if c.Attr[i].Namespace != c.Attr[j].Namespace {
						return c.Attr[i].Namespace < c.Attr[j].Namespace
					}
					return c.Attr[i].Key < c.Attr[j].Key
				})
			}

			if c.Type == html.TextNode && !item.preserve {
				c.Data = collapseWhitespace(c.Data)
				if c.Data == "" {
					node.RemoveChild(c)
					continue
				}
			}

			children = append(children, stackItem{node: c, preserve: childPreserve})
		}
		// Push in reverse so stack pops in DOM order.
		for i := len(children) - 1; i >= 0; i-- {
			stack = append(stack, children[i])
		}
	}
}

var whitespaceRegex = regexp.MustCompile(`\s+`)

func collapseWhitespace(s string) string {
	return strings.TrimSpace(whitespaceRegex.ReplaceAllString(s, " "))
}
