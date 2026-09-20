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
            <div v-if="group.nodes.length" class="inbound-list">
                <div v-for="node in group.nodes" :key="node.id" class="inbound-row">
                    <span class="inbound-id">#{{ node.id }}</span>
                    <span v-for="name in group.fields" :key="name" class="inbound-cell">
                        <component v-if="widgetOf(node, name)" :is="widgetOf(node, name)"
                                   mode="cell" :model-value="(node.fields || {})[name]"
                                   :field="fieldOf(node, name)" :defs="defs" :node="node" />
                    </span>
                </div>
            </div>
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
        // 单元格渲染: 用 kind 名取组件（与列表页同一套）, 取不到就留空
        widgetOf(node, name) {
            const field = this.fieldOf(node, name)
            return field && window.Widgets ? window.Widgets.resolve(field.kind) : null
        },
        fieldOf(node, name) {
            const def = this.defs && this.defs[node.type]
            const fields = (def && def.fields) || []
            return fields.find((f) => f.name === name) || null
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
