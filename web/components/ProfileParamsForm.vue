<!-- components/ProfileParamsForm.vue — Search Profile 可调参数表单
//
// 引入动机：管理端"搜索配置"页的"创建配置"弹窗与"新建版本"弹窗需要同一套可调参数输入控件。
// 此前两个弹窗各自内联渲染其中约 11 个字段，其余 14 个字段（embedding instruction、
// 全部 reranker 参数、analyzer、预算阈值等）只能继承、无法在界面上调整。
// 抽出本组件后：
//   - 字段定义只有一份，不会出现"创建能改、新建版本不能改"这类两处漂移；
//   - 25 个字段与 CreateProfileRequest / 后端 createProfileRequest 契约一一对应。
//
// 绑定方式：v-model 绑定一个完整的 25 字段表单对象（CreateProfileRequest）。
// 组件就地修改该对象的字段，而不复制副本：父组件持有的对象本身是 reactive 的，
// 因此输入即写回父状态，提交时直接读取父对象即可拿到最新值。
-->
<script setup lang="ts">
import type { CreateProfileRequest } from '~/types/api'

const { t } = useI18n()

/** 表单对象：与 CreateProfileRequest 一一对应的 25 个可调参数，由父组件以 v-model 传入。 */
const form = defineModel<CreateProfileRequest>({ required: true })
</script>

<template>
  <div class="space-y-5">
    <!-- 配置名称：profile 标识，不属于任何检索参数分组 -->
    <UFormField :label="t('workspace.name')" name="name">
      <UInput v-model="form.name" class="w-full" />
    </UFormField>

    <!-- Embedding：向量化模型与指令。两条 instruction 对检索质量影响最大，用多行文本编辑。 -->
    <section class="space-y-3">
      <h4 class="text-sm font-semibold text-highlighted">{{ t('admin.groupEmbedding') }}</h4>
      <div class="grid grid-cols-2 gap-3">
        <UFormField :label="t('admin.embeddingProvider')">
          <UInput v-model="form.embedding_provider" class="w-full" />
        </UFormField>
        <UFormField :label="t('admin.embeddingModel')">
          <UInput v-model="form.embedding_model" class="w-full" />
        </UFormField>
      </div>
      <UFormField :label="t('admin.dimensions')">
        <UInput v-model.number="form.embedding_dimensions" type="number" class="w-full" />
      </UFormField>
      <UFormField :label="t('admin.embeddingQueryInstruction')">
        <UTextarea v-model="form.embedding_query_instruction" :rows="2" class="w-full" />
      </UFormField>
      <UFormField :label="t('admin.embeddingDocumentInstruction')">
        <UTextarea v-model="form.embedding_document_instruction" :rows="2" class="w-full" />
      </UFormField>
    </section>

    <!-- Chunk：切分粒度 -->
    <section class="space-y-3">
      <h4 class="text-sm font-semibold text-highlighted">{{ t('admin.groupChunk') }}</h4>
      <div class="grid grid-cols-2 gap-3">
        <UFormField :label="t('admin.chunkTargetSize')">
          <UInput v-model.number="form.chunk_target_size" type="number" class="w-full" />
        </UFormField>
        <UFormField :label="t('admin.chunkOverlap')">
          <UInput v-model.number="form.chunk_overlap" type="number" class="w-full" />
        </UFormField>
      </div>
    </section>

    <!-- Lexical boosts：词法检索的字段权重与分词器 -->
    <section class="space-y-3">
      <h4 class="text-sm font-semibold text-highlighted">{{ t('admin.groupLexicalBoosts') }}</h4>
      <div class="grid grid-cols-3 gap-3">
        <UFormField :label="t('admin.titleBoost')">
          <UInput v-model.number="form.title_boost" type="number" step="0.1" class="w-full" />
        </UFormField>
        <UFormField :label="t('admin.headingBoost')">
          <UInput v-model.number="form.heading_boost" type="number" step="0.1" class="w-full" />
        </UFormField>
        <UFormField :label="t('admin.pathBoost')">
          <UInput v-model.number="form.path_boost" type="number" step="0.1" class="w-full" />
        </UFormField>
      </div>
      <div class="grid grid-cols-3 gap-3">
        <UFormField :label="t('admin.tagsBoost')">
          <UInput v-model.number="form.tags_boost" type="number" step="0.1" class="w-full" />
        </UFormField>
        <UFormField :label="t('admin.bodyBoost')">
          <UInput v-model.number="form.body_boost" type="number" step="0.1" class="w-full" />
        </UFormField>
        <!-- 分词器名为 Elasticsearch analyzer 标识（后端不做白名单校验），因此用自由文本而非固定下拉 -->
        <UFormField :label="t('admin.analyzer')">
          <UInput v-model="form.analyzer" placeholder="standard" class="w-full" />
        </UFormField>
      </div>
    </section>

    <!-- Retrieval：召回条数与 RRF 融合参数 -->
    <section class="space-y-3">
      <h4 class="text-sm font-semibold text-highlighted">{{ t('admin.groupRetrieval') }}</h4>
      <div class="grid grid-cols-3 gap-3">
        <UFormField :label="t('admin.lexicalTopK')">
          <UInput v-model.number="form.lexical_top_k" type="number" class="w-full" />
        </UFormField>
        <UFormField :label="t('admin.vectorTopK')">
          <UInput v-model.number="form.vector_top_k" type="number" class="w-full" />
        </UFormField>
        <UFormField :label="t('admin.rrfK')">
          <UInput v-model.number="form.rrf_k" type="number" class="w-full" />
        </UFormField>
      </div>
    </section>

    <!-- Reranker：重排模型与候选/最终条数 -->
    <section class="space-y-3">
      <h4 class="text-sm font-semibold text-highlighted">{{ t('admin.groupReranker') }}</h4>
      <div class="grid grid-cols-2 gap-3">
        <UFormField :label="t('admin.rerankerProvider')">
          <UInput v-model="form.reranker_provider" class="w-full" />
        </UFormField>
        <UFormField :label="t('admin.rerankerModel')">
          <UInput v-model="form.reranker_model" class="w-full" />
        </UFormField>
      </div>
      <div class="grid grid-cols-2 gap-3">
        <UFormField :label="t('admin.rerankerCandidateCount')">
          <UInput v-model.number="form.reranker_candidate_count" type="number" class="w-full" />
        </UFormField>
        <UFormField :label="t('admin.rerankerFinalCount')">
          <UInput v-model.number="form.reranker_final_count" type="number" class="w-full" />
        </UFormField>
      </div>
    </section>

    <!-- Diversification：结果多样化 -->
    <section class="space-y-3">
      <h4 class="text-sm font-semibold text-highlighted">{{ t('admin.groupDiversification') }}</h4>
      <div class="grid grid-cols-2 gap-3">
        <UFormField :label="t('admin.maxChunksPerDocument')">
          <UInput v-model.number="form.max_chunks_per_document" type="number" class="w-full" />
        </UFormField>
        <UFormField :label="t('admin.mergeAdjacentChunks')">
          <USwitch v-model="form.merge_adjacent_chunks" />
        </UFormField>
      </div>
    </section>

    <!-- Budget：检索质量以外的硬约束 -->
    <section class="space-y-3">
      <h4 class="text-sm font-semibold text-highlighted">{{ t('admin.groupBudget') }}</h4>
      <div class="grid grid-cols-2 gap-3">
        <UFormField :label="t('admin.maxP95LatencyMs')">
          <UInput v-model.number="form.max_p95_latency_ms" type="number" class="w-full" />
        </UFormField>
        <UFormField :label="t('admin.maxRerankerCostPerQuery')">
          <UInput v-model.number="form.max_reranker_cost_per_query" type="number" step="0.001" class="w-full" />
        </UFormField>
      </div>
    </section>
  </div>
</template>
