// chunking_test.go 测试 Markdown-aware chunking 逻辑。
//
// 引入动机：design/06-IMPLEMENTATION.md §Tests 要求 lexical search, semantic search,
// RRF, reranker fallback, document diversity 等测试。
// 本文件测试 chunking 的纯逻辑：section 拆分、超长 section child chunk、content_hash 正确性。
package chunking

import (
	"strings"
	"testing"

	types "partitura/server/internal/search/types"
)

func TestChunkDocument_NoHeading(t *testing.T) {
	doc := DocumentInput{
		DocumentID:  "doc-1",
		WorkspaceID: "ws-1",
		Path:        "notes.md",
		Title:       "Notes",
		Content:     "This is a simple document without headings.",
		Revision:    1,
		Status:      "active",
	}

	chunks := ChunkDocument(doc, DefaultTargetSize, DefaultOverlap)
	if len(chunks) != 1 {
		t.Fatalf("无 heading 的文档应有 1 个 chunk，得到 %d", len(chunks))
	}
	if chunks[0].Content != "This is a simple document without headings." {
		t.Errorf("chunk 内容不匹配: %s", chunks[0].Content)
	}
	if chunks[0].ChunkIndex != 0 {
		t.Errorf("chunk index 应为 0，得到 %d", chunks[0].ChunkIndex)
	}
	if chunks[0].DocumentID != "doc-1" {
		t.Errorf("document_id 不匹配")
	}
	if chunks[0].ContentHash == "" {
		t.Error("content_hash 不应为空")
	}
}

func TestChunkDocument_WithHeadings(t *testing.T) {
	content := `# Architecture

This is the architecture section.

## Components

Component details here.

## Database

Database details here.
`
	doc := DocumentInput{
		DocumentID:  "doc-2",
		WorkspaceID: "ws-1",
		Path:        "architecture/overview.md",
		Title:       "Architecture Overview",
		Content:     content,
		Revision:    1,
		Status:      "active",
	}

	chunks := ChunkDocument(doc, DefaultTargetSize, DefaultOverlap)
	if len(chunks) < 3 {
		t.Fatalf("有 3 个 heading 的文档应至少有 3 个 chunk，得到 %d", len(chunks))
	}

	// 验证每个 chunk 有正确的 section_path
	found := make(map[string]bool)
	for _, c := range chunks {
		key := strings.Join(c.SectionPath, " > ")
		found[key] = true
	}

	if !found["Architecture"] {
		t.Error("缺少 Architecture section")
	}
	if !found["Architecture > Components"] {
		t.Error("缺少 Architecture > Components section")
	}
	if !found["Architecture > Database"] {
		t.Error("缺少 Architecture > Database section")
	}
}

func TestChunkDocument_LongSectionSplit(t *testing.T) {
	// 创建一个超长 section
	longContent := strings.Repeat("This is a long line of content. ", 100)
	content := "# Long Section\n\n" + longContent + "\n"

	doc := DocumentInput{
		DocumentID:  "doc-3",
		WorkspaceID: "ws-1",
		Path:        "long.md",
		Title:       "Long",
		Content:     content,
		Revision:    1,
		Status:      "active",
	}

	chunks := ChunkDocument(doc, 200, 50)
	if len(chunks) < 2 {
		t.Fatalf("超长 section 应拆分为多个 child chunk，得到 %d", len(chunks))
	}

	// 验证所有 chunk 都有正确的 heading
	for _, c := range chunks {
		if c.Heading != "Long Section" {
			t.Errorf("child chunk heading 应为 'Long Section'，得到 '%s'", c.Heading)
		}
	}

	// 验证 chunk_index 递增
	for i, c := range chunks {
		if c.ChunkIndex != i {
			t.Errorf("chunk %d 的 chunk_index 应为 %d，得到 %d", i, i, c.ChunkIndex)
		}
	}
}

func TestChunkDocument_EmptyContent(t *testing.T) {
	doc := DocumentInput{
		DocumentID:  "doc-4",
		WorkspaceID: "ws-1",
		Path:        "empty.md",
		Title:       "Empty",
		Content:     "",
	}

	chunks := ChunkDocument(doc, DefaultTargetSize, DefaultOverlap)
	if len(chunks) != 0 {
		t.Errorf("空文档应返回 0 个 chunk，得到 %d", len(chunks))
	}
}

func TestChunkDocument_ContentHashStability(t *testing.T) {
	doc := DocumentInput{
		DocumentID:  "doc-5",
		WorkspaceID: "ws-1",
		Path:        "test.md",
		Title:       "Test",
		Content:     "# Test\n\nContent here.",
		Revision:    1,
		Status:      "active",
	}

	chunks1 := ChunkDocument(doc, DefaultTargetSize, DefaultOverlap)
	chunks2 := ChunkDocument(doc, DefaultTargetSize, DefaultOverlap)

	if len(chunks1) != len(chunks2) {
		t.Fatalf("相同输入应产生相同数量的 chunk")
	}

	for i := range chunks1 {
		if chunks1[i].ContentHash != chunks2[i].ContentHash {
			t.Errorf("chunk %d 的 content_hash 不稳定", i)
		}
	}
}

func TestChunkDocument_SpecialFileFlag(t *testing.T) {
	doc := DocumentInput{
		DocumentID:  "doc-6",
		WorkspaceID: "ws-1",
		Path:        "PROJECT.md",
		Title:       "Project",
		Content:     "# Project\n\nDescription.",
		IsSpecial:   true,
	}

	chunks := ChunkDocument(doc, DefaultTargetSize, DefaultOverlap)
	for _, c := range chunks {
		if !c.IsSpecial {
			t.Error("特殊文件的 chunk 应标记 is_special=true")
		}
	}
}

func TestBuildEmbeddingInput(t *testing.T) {
	chunk := types.Chunk{
		Title:       "Architecture",
		Path:        "architecture/overview.md",
		SectionPath: []string{"Architecture", "Components"},
		Content:     "The database uses PostgreSQL.",
	}

	input := BuildEmbeddingInput(chunk, "", "Represent this document:")
	if !strings.Contains(input, "Architecture") {
		t.Error("embedding input 应包含 title")
	}
	if !strings.Contains(input, "architecture/overview.md") {
		t.Error("embedding input 应包含 path")
	}
	if !strings.Contains(input, "Architecture > Components") {
		t.Error("embedding input 应包含 section_path")
	}
	if !strings.Contains(input, "The database uses PostgreSQL.") {
		t.Error("embedding input 应包含 content")
	}
	if !strings.Contains(input, "Represent this document:") {
		t.Error("embedding input 应包含 document instruction")
	}
}

func TestBuildQueryEmbeddingInput(t *testing.T) {
	input := BuildQueryEmbeddingInput("how does the database work?", "Encode the query:")
	if !strings.Contains(input, "Encode the query:") {
		t.Error("query embedding input 应包含 query instruction")
	}
	if !strings.Contains(input, "how does the database work?") {
		t.Error("query embedding input 应包含 query")
	}
}
