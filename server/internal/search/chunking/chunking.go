// Package chunking 实现 Markdown-aware 文档分片：Document → Section → Child Chunk。
//
// 引入动机：design/01-SEARCH.md §Chunking 要求：
//   - 优先按照 heading 构造 Section
//   - 超长 Section 才拆 child chunk
//   - 每个 child 保存 document_id, section_path, start_line, end_line, chunk_index, content_hash
//   - Embedding 输入建议：Document title + Path + Section hierarchy + Chunk content
//   - 不要把 Workspace 名作为主要 embedding 文本
//
// 设计原则：
//   - 核心解析逻辑受控不崩溃——畸形输入只产生空或部分结果
//   - chunk 大小按字符数控制，overlap 控制重叠
//   - 特殊文件（PROJECT.md/AGENTS.md）在调用层排除，chunking 本身不跳过
package chunking

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	types "partitura/server/internal/search/types"
)

// DefaultTargetSize 是默认的 chunk 目标大小（字符数）。
// 引入动机：design/01-SEARCH.md §Chunking 要求超长 section 才拆 child chunk，
// 512 字符是合理的 chunk 大小，兼顾检索精度和 embedding 输入长度。
const DefaultTargetSize = 512

// DefaultOverlap 是默认的 chunk 重叠大小（字符数）。
// 引入动机：overlap 保证 chunk 边界处的上下文不丢失。
const DefaultOverlap = 64

// ChunkDocument 将 Markdown 文档按 heading 分为 Section，超长 Section 拆分为 Child Chunk。
//
// 引入动机：design/01-SEARCH.md §Chunking 要求 Markdown-aware chunking，
// Document → Section → Child Chunk。
//
// 参数：
//   - doc：文档信息（ID, workspace, path, title, content, revision, status, isSpecial）
//   - targetSize：chunk 目标大小（字符数），超长 section 按此大小拆分
//   - overlap：chunk 之间的重叠字符数
//
// 返回 chunk 列表。如果文档为空或无 heading，整个文档作为一个 chunk。
func ChunkDocument(doc DocumentInput, targetSize, overlap int) []types.Chunk {
	if targetSize <= 0 {
		targetSize = DefaultTargetSize
	}
	if overlap < 0 {
		overlap = 0
	}

	content := doc.Content
	if content == "" {
		return nil
	}

	lines := strings.Split(content, "\n")

	// 第一遍：识别所有 heading 及其行号范围
	sections := parseSections(lines)

	var chunks []types.Chunk
	chunkIndex := 0

	if len(sections) == 0 {
		// 无 heading，整个文档作为一个 section
		fullContent := content
		chunks = append(chunks, makeChunk(doc, "", []string{}, fullContent, 1, len(lines), chunkIndex))
		return chunks
	}

	// 处理文档头部到第一个 heading 之前的内容（pre-section）
	firstHeadingLine := sections[0].startLine
	if firstHeadingLine > 1 {
		preContent := strings.Join(lines[0:firstHeadingLine-1], "\n")
		preContent = strings.TrimSpace(preContent)
		if preContent != "" {
			subChunks := splitIfTooLong(doc, "", []string{}, preContent, 1, firstHeadingLine-1, &chunkIndex, targetSize, overlap)
			chunks = append(chunks, subChunks...)
		}
	}

	// 处理每个 section
	for i, sec := range sections {
		secContent := strings.Join(lines[sec.startLine-1:sec.endLine], "\n")
		secContent = strings.TrimSpace(secContent)
		if secContent == "" {
			continue
		}

		// 如果 section 内容不超过 targetSize，整段作为一个 chunk
		if len(secContent) <= targetSize {
			chunks = append(chunks, makeChunk(doc, sec.text, sec.sectionPath, secContent, sec.startLine, sec.endLine, chunkIndex))
			chunkIndex++
		} else {
			// 超长 section 拆分为 child chunk
			subChunks := splitIfTooLong(doc, sec.text, sec.sectionPath, secContent, sec.startLine, sec.endLine, &chunkIndex, targetSize, overlap)
			chunks = append(chunks, subChunks...)
		}

		// 处理 section 之间的空隙（如果有内容在两个 section 之间但不在任何 section 内）
		if i < len(sections)-1 {
			nextStart := sections[i+1].startLine
			if sec.endLine+1 < nextStart {
				gapContent := strings.Join(lines[sec.endLine:nextStart-1], "\n")
				gapContent = strings.TrimSpace(gapContent)
				if gapContent != "" {
					gapChunks := splitIfTooLong(doc, "", []string{}, gapContent, sec.endLine+1, nextStart-1, &chunkIndex, targetSize, overlap)
					chunks = append(chunks, gapChunks...)
				}
			}
		}
	}

	// 处理最后一个 section 之后的内容
	lastEnd := sections[len(sections)-1].endLine
	if lastEnd < len(lines) {
		tailContent := strings.Join(lines[lastEnd:], "\n")
		tailContent = strings.TrimSpace(tailContent)
		if tailContent != "" {
			tailChunks := splitIfTooLong(doc, "", []string{}, tailContent, lastEnd+1, len(lines), &chunkIndex, targetSize, overlap)
			chunks = append(chunks, tailChunks...)
		}
	}

	return chunks
}

// DocumentInput 是 chunking 的输入参数。
// 引入动机：chunking 需要文档的元数据和内容来生成 chunk。
type DocumentInput struct {
	DocumentID  string
	WorkspaceID string
	Path        string
	Title       string
	Content     string
	Revision    int
	Status      string
	IsSpecial   bool
}

// section 表示一个 Markdown section 的行范围和路径信息。
type section struct {
	text        string
	level       int
	startLine   int
	endLine     int
	sectionPath []string
}

// parseSections 解析 Markdown 行数组，返回 section 列表。
// 引入动机：与 document.ParseOutline 类似但返回内部 section 结构，
// 避免跨包依赖。
func parseSections(lines []string) []section {
	type rawHeading struct {
		level     int
		text      string
		startLine int
	}
	var raws []rawHeading

	for i, line := range lines {
		level, text, ok := parseATXHeading(line)
		if !ok {
			continue
		}
		raws = append(raws, rawHeading{
			level:     level,
			text:      text,
			startLine: i + 1,
		})
	}

	if len(raws) == 0 {
		return nil
	}

	type stackEntry struct {
		level int
		text  string
		index int
	}
	var stack []stackEntry
	var sections []section

	for i, rh := range raws {
		for len(stack) > 0 && stack[len(stack)-1].level >= rh.level {
			stack = stack[:len(stack)-1]
		}

		sectionPath := make([]string, 0, len(stack)+1)
		for _, entry := range stack {
			sectionPath = append(sectionPath, entry.text)
		}
		sectionPath = append(sectionPath, rh.text)

		endLine := len(lines)
		for j := i + 1; j < len(raws); j++ {
			if raws[j].level <= rh.level {
				endLine = raws[j].startLine - 1
				break
			}
		}

		sections = append(sections, section{
			text:        rh.text,
			level:       rh.level,
			startLine:   rh.startLine,
			endLine:     endLine,
			sectionPath: sectionPath,
		})

		stack = append(stack, stackEntry{
			level: rh.level,
			text:  rh.text,
			index: i,
		})
	}

	return sections
}

// splitIfTooLong 将超长文本按 targetSize 拆分为多个 child chunk，带 overlap。
// 引入动机：design/01-SEARCH.md §Chunking 要求超长 Section 才拆 child chunk。
func splitIfTooLong(doc DocumentInput, heading string, sectionPath []string, content string, startLine, endLine int, chunkIndex *int, targetSize, overlap int) []types.Chunk {
	if len(content) <= targetSize {
		chunk := makeChunk(doc, heading, sectionPath, content, startLine, endLine, *chunkIndex)
		*chunkIndex++
		return []types.Chunk{chunk}
	}

	var chunks []types.Chunk
	// 按行拆分以避免在词中间截断
	contentLines := strings.Split(content, "\n")

	var currentLines []string
	currentLen := 0
	currentStart := startLine

	for i, line := range contentLines {
		lineLen := len(line) + 1 // +1 for newline
		if currentLen+lineLen > targetSize && len(currentLines) > 0 {
			// 当前 chunk 已满，创建 chunk
			chunkText := strings.Join(currentLines, "\n")
			chunkEnd := currentStart + len(currentLines) - 1
			chunks = append(chunks, makeChunk(doc, heading, sectionPath, chunkText, currentStart, chunkEnd, *chunkIndex))
			*chunkIndex++

			// overlap：保留尾部部分行
			overlapLen := 0
			overlapStart := len(currentLines)
			for overlapStart > 0 && overlapLen < overlap {
				overlapStart--
				overlapLen += len(currentLines[overlapStart]) + 1
			}
			currentLines = currentLines[overlapStart:]
			currentLen = overlapLen
			currentStart = currentStart + overlapStart
		}
		currentLines = append(currentLines, line)
		currentLen += lineLen
		_ = i
	}

	if len(currentLines) > 0 {
		chunkText := strings.Join(currentLines, "\n")
		chunkEnd := currentStart + len(currentLines) - 1
		if chunkEnd > endLine {
			chunkEnd = endLine
		}
		chunks = append(chunks, makeChunk(doc, heading, sectionPath, chunkText, currentStart, chunkEnd, *chunkIndex))
		*chunkIndex++
	}

	return chunks
}

// makeChunk 创建一个 types.Chunk。
func makeChunk(doc DocumentInput, heading string, sectionPath []string, content string, startLine, endLine, chunkIndex int) types.Chunk {
	return types.Chunk{
		DocumentID:  doc.DocumentID,
		WorkspaceID: doc.WorkspaceID,
		Path:        doc.Path,
		Title:       doc.Title,
		SectionPath: sectionPath,
		Heading:     heading,
		Content:     content,
		StartLine:   startLine,
		EndLine:     endLine,
		ChunkIndex:  chunkIndex,
		ContentHash: computeHash(content),
		Revision:    doc.Revision,
		Status:      doc.Status,
		IsSpecial:   doc.IsSpecial,
	}
}

// computeHash 计算内容的 SHA-256 十六进制摘要。
func computeHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// parseATXHeading 解析单行是否为 ATX heading。
// 与 document.parseATXHeading 逻辑一致，独立实现以避免跨包依赖。
func parseATXHeading(line string) (int, string, bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return 0, "", false
	}

	hashCount := 0
	for hashCount < len(trimmed) && trimmed[hashCount] == '#' {
		hashCount++
	}
	if hashCount == 0 || hashCount > 6 {
		return 0, "", false
	}

	rest := trimmed[hashCount:]
	if len(rest) == 0 {
		return hashCount, "", true
	}
	if rest[0] != ' ' && rest[0] != '\t' {
		return 0, "", false
	}

	text := strings.TrimSpace(rest)
	text = removeTrailingHashes(text)

	return hashCount, text, true
}

// removeTrailingHashes 去除 heading text 尾部的 # 序列。
func removeTrailingHashes(text string) string {
	if text == "" {
		return text
	}
	end := len(text)
	for end > 0 && text[end-1] == '#' {
		end--
	}
	if end == len(text) {
		return text
	}
	if end > 0 && (text[end-1] == ' ' || text[end-1] == '\t') {
		return strings.TrimRight(text[:end], " \t")
	}
	return text
}

// BuildEmbeddingInput 根据 chunk 信息构建 embedding 输入文本。
// 引入动机：design/01-SEARCH.md §Chunking 建议 embedding 输入为
// Document title + Path + Section hierarchy + Chunk content，
// 不把 Workspace 名作为主要 embedding 文本。
func BuildEmbeddingInput(chunk types.Chunk, queryInstruction, docInstruction string) string {
	var parts []string
	if docInstruction != "" {
		parts = append(parts, docInstruction)
	}
	if chunk.Title != "" {
		parts = append(parts, chunk.Title)
	}
	if chunk.Path != "" {
		parts = append(parts, chunk.Path)
	}
	if len(chunk.SectionPath) > 0 {
		parts = append(parts, strings.Join(chunk.SectionPath, " > "))
	}
	if chunk.Content != "" {
		parts = append(parts, chunk.Content)
	}
	return strings.Join(parts, "\n")
}

// BuildQueryEmbeddingInput 构建查询的 embedding 输入文本。
// 引入动机：query 和 document 使用不同的 instruction，design/01-SEARCH.md §Embedding Provider。
func BuildQueryEmbeddingInput(query, queryInstruction string) string {
	if queryInstruction != "" {
		return queryInstruction + "\n" + query
	}
	return query
}
