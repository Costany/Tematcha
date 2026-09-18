// ui_diff.go —— edit__apply_patch 的 diff 特例渲染（M6 · §1.4）。
//
// 数据：rawInput {"edits":[{path,find,replace,replace_all},...]}
// （形状取证自 letcode src/tool/apply_patch.rs 的 parse_apply_patch）。
//
// 呈现：对齐 letcode diff_render.rs 的语义，但遵守透明度原则（只用前景色、
// 不铺底色）——旧行红系 / 新行绿系 + 行首 -/+ 标记；宽度够时双栏对照
// （左旧右新、中间 │，每侧带预览行号），窄时退化成单栏。
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// patchEdit 一条编辑：find（旧文）→ replace（新文）。
type patchEdit struct {
	Path    string
	Find    string
	Replace string
}

const (
	// diffMaxLines diff 展开区的最大行数（diff 重点在前，截尾）。
	diffMaxLines = 24
	// diffWideMin 双栏对照的最小总宽（再窄就单栏）。
	diffWideMin = 50
)

// patchEditsFromRaw 解析 edit__apply_patch 的 rawInput；不可解析返回 nil。
func patchEditsFromRaw(raw string) []patchEdit {
	if raw == "" || !strings.Contains(raw, "\"edits\"") {
		return nil
	}
	var m struct {
		Edits []struct {
			Path    string `json:"path"`
			Find    string `json:"find"`
			Replace string `json:"replace"`
		} `json:"edits"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	out := make([]patchEdit, 0, len(m.Edits))
	for _, e := range m.Edits {
		out = append(out, patchEdit{Path: e.Path, Find: e.Find, Replace: e.Replace})
	}
	return out
}

// hasPatchEdits 是否有可渲染的 diff 数据。
func hasPatchEdits(raw string) bool {
	return len(patchEditsFromRaw(raw)) > 0
}

// patchDiffLines 渲染 diff 展开区：每文件一块（← Patch <path> + diff 行）。
// w = 消息区宽（含行首 2 格缩进，同 toolBodyLines 的口径）。
func patchDiffLines(raw string, w int) []string {
	edits := patchEditsFromRaw(raw)
	if len(edits) == 0 || w < 20 {
		return nil
	}
	var paths []string
	byPath := map[string][]patchEdit{}
	for _, e := range edits {
		if _, ok := byPath[e.Path]; !ok {
			paths = append(paths, e.Path)
		}
		byPath[e.Path] = append(byPath[e.Path], e)
	}
	total := len(diffLinesForEdits(edits))
	if total == 0 {
		return nil
	}
	var out []string
	left := diffMaxLines
	for _, path := range paths {
		if left < 2 {
			break
		}
		out = append(out, "    "+dimStyle.Render("\u2190 Patch "+path))
		left--
		lines := diffLinesForEdits(byPath[path])
		if len(lines) > left {
			lines = lines[:left]
		}
		if w >= diffWideMin {
			out = append(out, diffSideBySide(lines, w)...)
		} else {
			out = append(out, diffSingleColumn(lines, w)...)
		}
		left -= len(lines)
	}
	if total > diffMaxLines {
		out = append(out, "    "+dimStyle.Render(fmt.Sprintf("\u2026 共 %d 行 diff · 已截断", total)))
	}
	return out
}

// diffLinesForEdits 把一组 edit 摊平成 diff 行（- 旧 / + 新）。
func diffLinesForEdits(edits []patchEdit) []string {
	var out []string
	for _, e := range edits {
		if e.Find != "" {
			for _, ln := range strings.Split(e.Find, "\n") {
				out = append(out, "-"+ln)
			}
		}
		if e.Replace != "" {
			for _, ln := range strings.Split(e.Replace, "\n") {
				out = append(out, "+"+ln)
			}
		}
	}
	return out
}

// diffLineStyle 行样式：旧行红系 / 新行绿系 / 其余暗色。
func diffLineStyle(line string) lipgloss.Style {
	switch {
	case strings.HasPrefix(line, "-"):
		return toolErStyle
	case strings.HasPrefix(line, "+"):
		return toolOKStyle
	default:
		return toolOutStyle
	}
}

// diffSingleColumn 单栏 diff：│ 竖条 + 染色行（与输出 tail 同款竖条）。
func diffSingleColumn(lines []string, w int) []string {
	segW := w - 4
	if segW < 8 {
		segW = 8
	}
	var out []string
	for _, ln := range lines {
		st := diffLineStyle(ln)
		for _, seg := range wrapText(ln, segW) {
			out = append(out, "  "+dimStyle.Render("\u2502")+" "+st.Render(seg))
		}
	}
	return out
}

// diffSideBySide 双栏对照：连续 - 行与随后连续 + 行配对；左旧右新。
// 每侧格式：`NNN - 文本`（N 为预览序号，空白侧补空格）。
func diffSideBySide(lines []string, w int) []string {
	pw := (w - 5) / 2 // 单侧宽："  " + pw + " │ " + pw
	if pw < 20 {
		return diffSingleColumn(lines, w)
	}
	inner := pw - 6 // 行号 3 + 空格 1 + 标记 1 + 空格 1 之后留给正文的宽
	if inner < 6 {
		inner = 6
	}
	var out []string
	i := 0
	for i < len(lines) {
		if strings.HasPrefix(lines[i], "-") {
			var removed, added []string
			for i < len(lines) && strings.HasPrefix(lines[i], "-") {
				removed = append(removed, lines[i])
				i++
			}
			for i < len(lines) && strings.HasPrefix(lines[i], "+") {
				added = append(added, lines[i])
				i++
			}
			n := len(removed)
			if len(added) > n {
				n = len(added)
			}
			for k := 0; k < n; k++ {
				l, lno := "", 0
				if k < len(removed) {
					l, lno = removed[k], k+1
				}
				r, rno := "", 0
				if k < len(added) {
					r, rno = added[k], k+1
				}
				out = append(out, "  "+diffCell(l, lno, pw, inner)+dimStyle.Render(" \u2502 ")+diffCell(r, rno, pw, inner))
			}
			continue
		}
		if strings.HasPrefix(lines[i], "+") {
			out = append(out, "  "+diffCell("", 0, pw, inner)+dimStyle.Render(" \u2502 ")+diffCell(lines[i], 1, pw, inner))
			i++
			continue
		}
		out = append(out, "  "+diffCell("", 0, pw, inner)+dimStyle.Render(" \u2502 ")+diffCell("", 0, pw, inner))
		i++
	}
	return out
}

// diffCell 渲染双栏中的一侧（空侧补满空格）。
func diffCell(side string, no, pw, inner int) string {
	if side == "" {
		return strings.Repeat(" ", pw)
	}
	st := diffLineStyle(side)
	mark, body := "-", side
	if strings.HasPrefix(side, "-") || strings.HasPrefix(side, "+") {
		mark, body = side[:1], side[1:]
	}
	num := "   "
	if no > 0 {
		num = fmt.Sprintf("%3d", no)
	}
	body = clipWidth(body, inner)
	pad := inner - lipgloss.Width(body)
	if pad < 0 {
		pad = 0
	}
	return dimStyle.Render(num) + " " + st.Render(mark+" "+body) + strings.Repeat(" ", pad)
}
