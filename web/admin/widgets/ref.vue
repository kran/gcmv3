<!-- kind ref：单引用；编辑 = 可搜索选择（本地过滤候选），列表 = 接口展开的显示名
     （引用值存 edges 表，不在 fields 里）。
     文件名就是 kind 名（web/admin/widgets/ref.vue），mode="edit" 编辑 / mode="cell" 只读。 -->
<template>
    <span v-if="mode === 'cell'" class="w-cell">
        <a v-if="target" class="w-ref-link" href="#" @click.prevent="open">{{ target.label }}</a>
        <span v-else class="w-empty">—</span>
    </span>
    <el-select v-else :model-value="modelValue" filterable clearable
        placeholder="搜索并选择节点"
        style="width:100%" @visible-change="(open) => open && preload()"
        @update:model-value="emitValue($event)">
        <el-option v-for="o in options" :key="o.id" :label="o.label" :value="o.id" />
    </el-select>
</template>
<script>
export default {
    name: 'WRef',
    props: {
        modelValue: { default: undefined },
        mode: { type: String, default: 'edit' },      // edit | cell
        field: { type: Object, default: () => ({}) },
        defs: { type: Object, default: () => ({}) },  // 类型定义表（ref 显示名用）
        node: { type: Object, default: () => ({}) },  // 当前节点（引用标签从 node.expand 取）
    },
    emits: ['update:modelValue', 'open-node'],
    computed: {
        options() { return [...this.preset, ...this.found] },
        // 引用目标来自 node.expand（列表与详情接口同形，都是展开一层）
        expandOne() {
            const list = (this.node.expand || {})[this.field.name]
            return Array.isArray(list) ? list[0] : (list || null)
        },
        // 编辑回显：已选值显示标题而不是裸 id
        preset() {
            const one = this.expandOne
            return one ? [window.Widgets.refOption(one, this.defs)] : []
        },
        // cell 用：{id, type, label} —— 有 id 就能点开那个节点的编辑表单
        target() {
            const one = this.expandOne
            if (one) return window.Widgets.refOption(one, this.defs)
            return this.modelValue ? { id: this.modelValue, label: '#' + this.modelValue } : null
        },
    },
    data() { return { found: [], loading: false, loaded: false } },
    methods: {
        emitValue(v) { this.$emit('update:modelValue', v) },
        open() { if (this.target) this.$emit('open-node', this.target) },
        // 候选: 目标类型的一页节点（**没有检索端点** —— 内核不带全文检索; 上限见 README）。
        // 过滤由组件的 filterable 在本地做（候选量级是分类/地区这类小集合）。
        async load() {
            this.loaded = true   // 先置位: 下拉连点两次不该拉两遍
            this.loading = true
            try {
                const res = await window.$api.nodes(this.field.to, { page: 1, size: 100, sort: '-id' })
                this.found = (res.items || []).map(n => window.Widgets.refOption(n, this.defs))
            } catch (_) { this.found = [] }
            this.loading = false
        },
        // 返回 promise: 调用方 await 得到"候选真的到了"（而不是"发起了"）
        preload() { return this.loaded ? undefined : this.load() },
    },
}
</script>
