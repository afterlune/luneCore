package tool

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
)

// Small helpers over the x/net/html node tree, used by the web tools.

func parseHTMLPage(body []byte) (title, text string, err error) {
	document, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	if titleNode := findElement(document, "title"); titleNode != nil {
		title = nodeText(titleNode)
	}
	bodyNode := findElement(document, "body")
	if bodyNode == nil {
		bodyNode = document
	}
	text = nodeTextSkipping(bodyNode, map[string]bool{
		"script": true, "style": true, "noscript": true, "template": true,
	})
	return title, text, nil
}

func collectElements(node *html.Node, tag string, matches *[]*html.Node) {
	if node.Type == html.ElementNode && node.Data == tag {
		*matches = append(*matches, node)
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		collectElements(child, tag, matches)
	}
}

func findElement(node *html.Node, tag string) *html.Node {
	if node.Type == html.ElementNode && node.Data == tag {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if match := findElement(child, tag); match != nil {
			return match
		}
	}
	return nil
}

func findClassElement(node *html.Node, tag, class string) *html.Node {
	if node.Type == html.ElementNode && (tag == "" || node.Data == tag) && hasClass(node, class) {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if match := findClassElement(child, tag, class); match != nil {
			return match
		}
	}
	return nil
}

func hasClass(node *html.Node, class string) bool {
	for _, attr := range node.Attr {
		if attr.Key == "class" {
			for _, candidate := range strings.Fields(attr.Val) {
				if candidate == class {
					return true
				}
			}
		}
	}
	return false
}

func attribute(node *html.Node, name string) string {
	for _, attr := range node.Attr {
		if attr.Key == name {
			return attr.Val
		}
	}
	return ""
}

func nodeText(node *html.Node) string {
	return nodeTextSkipping(node, nil)
}

func nodeTextSkipping(node *html.Node, skip map[string]bool) string {
	var text strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.ElementNode && skip[current.Data] {
			return
		}
		if current.Type == html.TextNode {
			text.WriteString(current.Data)
			text.WriteByte(' ')
			return
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return strings.Join(strings.Fields(text.String()), " ")
}
