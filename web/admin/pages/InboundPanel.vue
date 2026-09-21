<!-- 反向引用列表（admin.inbounds）—— 只读。

     例如编辑一个分类时显示"引用它的文章": 声明写在类型上, 数据每次打开现查
     （走后台的 /admin/inbounds/{type}/{id}, 内部走读入口 ⇒ 读规则照旧生效,
     藏起来的节点不出现）。列按声明里的 fields 渲染 —— 复用列表页那套单元格,
     所以显示效果与内容列表一致。

     只读: 入边的所有权在对面（是文章在引用分类）, 这里不给增删。 -->
<template>
    <div v-if="groups.length" class="inbound-block">
        <div v-for="group in groups" :key="group.spec" class="inbound-group">
            <div class="fr-label">
                <span>{{ group.label || group.type }}</span>
                <span class="fr-kind">{{ group.spec }}</span>
                <span class="fr-kind">共 {{ group.total }} 条</span>
            </div>
            <!-- 表格与内容列表同一套（el-table + 单元格渲染 + 结构/错误兜底）,
                 只是**没有操作列** —— 入边的所有权在对面, 这里不给增删。 -->
            <el-table v-if="group.nodes.length" :data="group.nodes" size="small" style="width:100%;">
                <el-table-column label="ID" width="70">
                    <template #default="{ row: r }">
                        <span class="inbound-id">#{{ r.id }}</span>
                    </template>
                </el-table-column>
                <el-table-column v-for="name in group.fields" :key="name" :label="labelOf(group.type, name)"
                                 min-width="120" show-overflow-tooltip>
                    <template #default="{ row: r }">
                        <component v-if="cellOf(group.type, name)" :is="cellOf(group.type, name)"
                                   mode="cell" :model-value="(r.fields || {})[name]"
                                   :field="fieldOf(group.type, name)" :defs="defs" :node="r" />
                        <span v-else-if="isStruct(group.type, name)" class="cell-struct">{{ structSummary(r, name) }}</span>
                        <span v-else class="cell-error">字段 {{ name }} 没有 kind</span>
                    </template>
                </el-table-column>
            </el-table>
            <div v-else class="inbound-empty">还没有引用它的节点</div>
        </div>
    </div>
</template>
<script>
// 主窗体传 type / nodeId（以及 defs —— 单元格渲染要靠它把引用解析成名字）。
export default {
    name: 'InboundPanel',
    props: {
        type: { type: String, required: true },
        nodeId: { type: Number, required: true },
        defs: { type: Object, default: () => ({}) },
    },
    data() {
        return { groups: [] }
    },
    watch: {
        // 抽屉 DOM 是复用的（换节点不重新挂载）—— 必须跟着节点重新查, 否则显示上一个人的
        nodeId() { this.load() },
        type() { this.load() },
    },
    mounted() { this.load() },
    methods: {
        load() {
            this.groups = []
            // 静默失败: 这是附加面板, 拿不到就不显示（不该弹全局错误）
            window.$api.inbounds(this.type, this.nodeId).then((res) => {
                this.groups = res.items || []
            }).catch(() => {})
        },
        // 与内容列表同一套单元格口径（**类型显式传入** —— 每组入边的对方类型不同）:
        //   fieldOf     列 → 字段定义
        //   cellOf      kind 名 → 组件（取不到文件由 Widgets 显示错误块）
        //   isStruct    array/object 是**结构**（没有组件）, 列表里报个规模
        //   labelOf     表头用字段的中文名（没有就退回字段名）
        fieldOf(typeName, name) {
            const def = (this.defs && this.defs[typeName]) || {}
            return (def.fields || []).find((f) => f.name === name) || null
        },
        cellOf(typeName, name) {
            const field = this.fieldOf(typeName, name)
            return field && window.Widgets ? window.Widgets.resolve(field.kind) : null
        },
        isStruct(typeName, name) {
            const field = this.fieldOf(typeName, name)
            return !!field && (field.kind === 'array' || field.kind === 'object')
        },
        labelOf(typeName, name) {
            const field = this.fieldOf(typeName, name)
            return (field && field.label) || name
        },
        structSummary(row, name) {
            const value = (row.fields || {})[name]
            if (Array.isArray(value)) return '（' + value.length + ' 项）'
            if (value && typeof value === 'object') return '（' + Object.keys(value).length + ' 个键）'
            return '—'
        },
    },
}
</script>
<style>
.inbound-block { margin-top: 16px; border-top: 1px solid #eee; padding-top: 12px; }
.inbound-group { margin-bottom: 14px; }
.inbound-list { border: 1px solid #eee; border-radius: 0; overflow: hidden; }
.inbound-row { display: flex; align-items: center; gap: 12px; padding: 6px 10px; font-size: 13px; border-bottom: 1px solid #f5f5f5; }
.inbound-row:last-child { border-bottom: none; }
.inbound-row:hover { background: #fafbfc; }
.inbound-id { color: #bbb; font-size: 11px; min-width: 38px; }
.inbound-cell { color: #333; }
.inbound-empty { color: #a19f9d; font-size: 12px; font-style: italic; padding: 4px 0; }
.fr-kind { font-weight: 400; color: #aaa; font-size: 11px; margin-left: 6px; }
.fr-label { font-size: 13px; font-weight: 600; color: #444; margin-bottom: 6px; }
</style>
