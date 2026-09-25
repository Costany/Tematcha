// ui_syntax.go —— 工具命令的 shell 语法高亮。
//
// 高亮发生在折行之前；折行按 token 与 grapheme 的显示宽度进行，续行会
// 重开前一段的前景色。输出只含前景色，不引入工具卡背景。
package main

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/x/ansi"
)

var (
	shellLexerOnce  = lexers.Get("bash")
	shellStyleCache *chroma.Style
	shellStyleEpoch uint64
)

// shellSyntaxStyle 构造当前主题的透明 shell 高亮表，并按 themeEpoch 缓存。
func shellSyntaxStyle() *chroma.Style {
	if shellStyleCache != nil && shellStyleEpoch == themeEpoch {
		return shellStyleCache
	}

	t := activeTheme
	shellStyleCache = chroma.MustNewStyle("tematcha-"+t.Name, chroma.StyleEntries{
		chroma.Text:                t.Text,
		chroma.Error:               "bold " + t.Err,
		chroma.Comment:             t.Dim,
		chroma.CommentPreproc:      "bold " + t.Code,
		chroma.Keyword:             "bold " + t.Code,
		chroma.KeywordReserved:     "bold " + t.Code,
		chroma.KeywordType:         "bold " + t.Accent,
		chroma.Operator:            t.Emph,
		chroma.Punctuation:         t.Faint,
		chroma.Name:                t.Text,
		chroma.NameBuiltin:         "bold " + t.Accent,
		chroma.NameTag:             t.Reply,
		chroma.NameAttribute:       t.Emph,
		chroma.NameClass:           "bold " + t.Heading,
		chroma.NameConstant:        t.Reply,
		chroma.NameFunction:        "bold " + t.Link,
		chroma.NameOther:           t.TextHi,
		chroma.LiteralNumber:       t.Reply,
		chroma.LiteralString:       t.Warn,
		chroma.LiteralStringEscape: "bold " + t.Accent,
	})
	shellStyleEpoch = themeEpoch
	return shellStyleCache
}

// highlightShellCommand 用 bash lexer 给命令加主题化前景色。
func highlightShellCommand(command string) (string, bool) {
	if shellLexerOnce == nil || strings.TrimSpace(command) == "" {
		return "", false
	}

	tokens, err := chroma.Tokenise(shellLexerOnce, nil, command)
	if err != nil || len(tokens) == 0 {
		return "", false
	}

	style := shellSyntaxStyle()
	var b strings.Builder
	for _, token := range tokens {
		entry := style.Get(token.Type)
		st := lipgloss.NewStyle()
		if entry.Colour.IsSet() {
			st = st.Foreground(lipgloss.Color(entry.Colour.String()))
		}
		if entry.Bold == chroma.Yes {
			st = st.Bold(true)
		}
		if entry.Italic == chroma.Yes {
			st = st.Italic(true)
		}
		if entry.Underline == chroma.Yes {
			st = st.Underline(true)
		}
		b.WriteString(st.Render(token.Value))
	}
	return b.String(), true
}

// shellCommandLines 返回可直接加缩进前缀的命令行；每行显示宽不超过 width。
func shellCommandLines(command string, width int) []string {
	if width < 1 {
		width = 1
	}
	if shellLexerOnce == nil || strings.TrimSpace(command) == "" {
		lines := wrapText(command, width)
		for i := range lines {
			lines[i] = textStyle.Render(lines[i])
		}
		return lines
	}

	tokens, err := chroma.Tokenise(shellLexerOnce, nil, command)
	if err != nil || len(tokens) == 0 {
		lines := wrapText(command, width)
		for i := range lines {
			lines[i] = textStyle.Render(lines[i])
		}
		return lines
	}

	style := shellSyntaxStyle()
	lines := make([]string, 0, 4)
	var line strings.Builder
	lineWidth := 0
	flush := func() {
		lines = append(lines, line.String())
		line.Reset()
		lineWidth = 0
	}

	for _, token := range tokens {
		entry := style.Get(token.Type)
		st := lipgloss.NewStyle()
		if entry.Colour.IsSet() {
			st = st.Foreground(lipgloss.Color(entry.Colour.String()))
		}
		if entry.Bold == chroma.Yes {
			st = st.Bold(true)
		}
		if entry.Italic == chroma.Yes {
			st = st.Italic(true)
		}
		if entry.Underline == chroma.Yes {
			st = st.Underline(true)
		}

		rest := token.Value
		for rest != "" {
			cluster, clusterWidth := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
			if cluster == "\n" || cluster == "\r" {
				flush()
				rest = rest[len(cluster):]
				continue
			}
			if lineWidth > 0 && lineWidth+clusterWidth > width {
				flush()
			}
			if clusterWidth > width {
				cluster = ansi.Truncate(cluster, width, "")
				clusterWidth = ansi.StringWidth(cluster)
			}
			line.WriteString(st.Render(cluster))
			lineWidth += clusterWidth
			rest = rest[len(cluster):]
		}
	}
	if line.Len() > 0 || len(lines) == 0 {
		flush()
	}
	return lines
}
