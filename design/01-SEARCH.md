# Search System

## 目标

搜索优先追求长期准确度和企业多项目稳定性，而不是只做简单 vector search。

最终流程：

```text
query
  ├─ multi-field BM25
  └─ dense vector search
          ↓
         RRF
          ↓
     top candidates
          ↓
    online reranker
          ↓
 document diversification
          ↓
      final results
```

使用 Elasticsearch 统一承担：
- BM25
- dense vector
- metadata filter
- highlighting
- RRF
- search index

PostgreSQL 不承担主搜索。

## Embedding Provider

定义 Provider abstraction。

至少支持 OpenAI-compatible Embeddings API。

配置：
- base_url
- api_key
- model
- dimensions
- timeout
- batch_size
- query_instruction
- document_instruction

默认推荐 Qwen3 Embedding。

不写死：
- 模型名
- 维度
- Provider

## Reranker Provider

定义独立 Provider。

配置：
- base_url
- api_key
- model
- timeout
- max_candidates

默认推荐 Qwen3 Reranker。

Reranker 不可用时：
- 直接返回 RRF 结果
- 系统降级而不是搜索失败

## Search Profile

Search Profile 必须版本化。

至少包含：

Embedding:
- provider
- model
- dimensions
- query instruction
- document instruction

Chunk:
- algorithm_version
- target_size
- overlap
- parent_section behavior

Lexical:
- title boost
- heading boost
- path boost
- tags boost
- body boost
- analyzer settings

Retrieval:
- lexical_top_k
- vector_top_k
- rrf_k

Reranker:
- provider
- model
- candidate_count
- final_count

Diversification:
- max_chunks_per_document
- merge_adjacent_chunks

## Index Version

索引必须 versioned：

knowledge_v12
knowledge_v13

使用 alias：

knowledge_current

任何涉及以下变化：
- embedding model
- dimensions
- instructions
- chunk algorithm
- analyzer

都必须：
1. 创建新 Search Profile
2. 创建新 index
3. 全量 rebuild
4. 验证完整性
5. 跑 evaluation
6. alias 原子切换
7. 保留旧 index 一段时间
8. 后台删除旧 index

禁止原地破坏 active index。

## PROJECT.md / AGENTS.md

这两个文件完全排除搜索。

不进入：
- BM25
- dense vector
- reranker candidate
- evaluation result

## Chunking

Markdown-aware。

结构：

Document
  Section
    Child Chunk

优先按照 heading 构造 Section。

超长 Section 才拆 child chunk。

每个 child 保存：
- document_id
- section_path
- start_line
- end_line
- chunk_index
- content_hash

Embedding 输入建议：

Document title
Path
Section hierarchy
Chunk content

不要把 Workspace 名作为主要 embedding 文本。

## Search Result

返回：

- document_id
- path
- title
- section_path
- start_line
- end_line
- snippet
- score
- revision
- source freshness

knowledge_search 不返回整篇长文。

Agent 需要全文时使用 section / lines 工具。

## Document Diversity

避免 Top 10 都来自同一文档。

Rerank 后：
- group by document
- 限制单文档 chunk 数
- 合并相邻 chunk
- 保留多个不同文档

## Evaluation

必须建设 Search Evaluation Dataset。

每条：
- query
- expected/relevant documents
- relevance grade 0..3
- query class

Query class 示例：
- semantic_question
- identifier
- path
- error_message
- command
- architecture
- issue
- research

指标：
- NDCG@10
- Recall@5
- Recall@10
- MRR
- Precision@10
- P50/P95 latency
- reranker cost

## Feedback

允许记录：
- verified evaluation
- explicit feedback
- implicit search -> read / patch 信号

Verified Evaluation 权重最高。

不得保存 Agent chain-of-thought。

## Auto Tuning

自动调优分级：

Level 1:
可以自动创建 candidate 并在通过 regression gate 后自动激活：
- field boost
- top_k
- RRF 参数
- candidate count
- diversity 参数

Level 2:
自动测试但管理员确认激活：
- chunk size
- overlap
- embedding dimensions
- embedding instructions
- reranker config

Level 3:
必须人工触发：
- 更换 embedding model
- 更换 provider
- 更换 analyzer

## Regression Gate

Candidate 激活必须同时满足：

- overall quality 有明确提升
- Recall@10 不下降
- 任一关键 query class 不出现明显退化
- P95 latency 不超过阈值
- 成本不超过管理员预算
- index integrity 正常

必须支持立即回滚到旧 Search Profile。

## 定时维护

持续：
- 收集 search metrics
- 收集 feedback

每天：
- index integrity check
- missing/stale/orphan chunk repair
- failed job retry

每周：
- Level 1 candidate optimization
- evaluation replay

低频 / 管理员触发：
- full profile rebuild
- Level 2/3 experiments

不要为了“优化”无意义地定时重新 embedding 全库。

## Index Integrity

检查：
- PostgreSQL current documents vs ES documents
- content_hash
- revision
- chunk count
- index profile
- missing chunks
- orphan chunks
- stale chunks

发现差异只修复相关文档。

## Elasticsearch Maintenance

不要对持续写入 active index 定时暴力 force merge。

Rebuild 新 index 时允许优化：
- bulk indexing
- 临时调大 refresh interval
- build 完成后恢复
- warm queries
- verify
- alias switch
