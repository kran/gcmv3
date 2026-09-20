<!-- kind textarea：多行文本。
     文件名就是 kind 名（web/admin/widgets/textarea.vue）。
     mode = edit 编辑 / cell 列表单元格（压成一行截断）/ view 只读详情（**保留换行**）。 -->
<template>
    <span v-if="mode === 'cell'" class="w-cell">{{ Widgets.truncate(oneLine) }}</span>
    <span v-else-if="mode === 'view'" class="w-cell w-view">{{ modelValue }}</span>
    <el-input v-else type="textarea" :rows="4"
        :model-value="modelValue === undefined || modelValue === null ? '' : modelValue"
        @update:model-value="emitValue($event)" />
</template>
<script>
export default {
    name: 'WTextarea',
    props: {
        modelValue: { default: undefined },
        mode: { type: String, default: 'edit' },      // edit | cell | view
        field: { type: Object, default: () => ({}) },
        defs: { type: Object, default: () => ({}) },  // 类型定义表（ref 显示名用）
    },
    emits: ['update:modelValue'],
    computed: {
        oneLine() { return String(this.modelValue || "").replace(/\s+/g, " ") },
    },
    methods: {
        emitValue(v) { this.$emit('update:modelValue', v) },
    },
}
</script>
<style>
/* 只读详情（view）: 保留换行 —— 列表里（cell）才压成一行 */
.w-view { white-space: pre-wrap; }
</style>
