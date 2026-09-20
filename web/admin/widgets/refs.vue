<!-- kind refs：多引用；编辑 = 可搜索多选（本地过滤候选），列表 = 展开的显示名列表。
     文件名就是 kind 名（web/admin/widgets/refs.vue），mode="edit" 编辑 / mode="cell" 只读。 -->
<template>
    <span v-if="mode === 'cell'" class="w-cell w-refs">
        <template v-for="(t, i) in targets" :key="t.id">
            <a class="w-ref-link" href="#" @click.prevent="open(t)">{{ t.label }}</a><span
                v-if="i < targets.length - 1">、</span>
        </template>
        <span v-if="!targets.length" class="w-empty">—</span>
    </span>
    <el-select v-else :model-value="modelValue || []" multiple filterable
        placeholder="搜索并选择多个节点"
        style="width:100%" @visible-change="(open) => open && preload()"
        @update:model-value="emitValue($event)">
        <el-option v-for="o in options" :key="o.id" :label="o.label" :value="o.id" />
    </el-select>
</template>
<script>
export default {
    name: 'WRefs',
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
        // 引用目标来自 node.expand（列表与详情接口同形）
        refList() {
            let list = (this.node.expand || {})[this.field.name]
            if (!list) return []
            return Array.isArray(list) ? list.filter(Boolean) : [list]
        },
        preset() {
            return this.refList.map(n => window.Widgets.refOption(n, this.defs))
        },
        // cell 用：每个引用 {id, type, label}
        targets() {
            return this.refList.map(n => window.Widgets.refOption(n, this.defs))
        },
    },
    data() { return { found: [], loading: false, loaded: false } },
    methods: {
        emitValue(v) { this.$emit('update:modelValue', v) },
        open(t) { this.$emit('open-node', t) },
        // 同 ref.vue: 拉一页候选, 过滤交给组件的 filterable（没有检索端点）
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
<style>
/* 多个引用：一行放不下就换行 */
.w-refs { flex-wrap: wrap; }
</style>
