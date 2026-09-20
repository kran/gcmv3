<!-- kind address：节点的地址（URL 段）。
     文件名就是 kind 名（web/admin/widgets/address.vue），mode="edit" 编辑 / mode="cell" 只读。
     这个字段由 capabilities.addressable 注入（站点声明不了）—— 全表唯一, 留空表示"没有地址"。 -->
<template>
    <span v-if="mode === 'cell'" class="w-cell w-slug">{{ modelValue }}</span>
    <el-input v-else :model-value="modelValue === undefined || modelValue === null ? '' : modelValue"
        placeholder="URL 段, 如 about-us（留空 = 没有地址）" @update:model-value="emitValue($event)" />
</template>
<script>
export default {
    name: 'WAddress',
    props: {
        modelValue: { default: undefined },
        mode: { type: String, default: 'edit' },      // edit | cell
        field: { type: Object, default: () => ({}) },
        defs: { type: Object, default: () => ({}) },
    },
    emits: ['update:modelValue'],
    methods: {
        emitValue(v) { this.$emit('update:modelValue', v) },
    },
}
</script>
<style>
/* URL 段：列表里显示成等宽 code */
.w-slug { font-size: 11px; color: #444; }
</style>
