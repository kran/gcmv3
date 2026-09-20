<!-- kind text：单行文本。
     文件名就是 kind 名（web/admin/widgets/text.vue）。
     mode = edit 编辑 / cell 列表单元格（截断）/ view 只读详情（**完整**, 不截断）。 -->
<template>
    <span v-if="mode === 'cell'" class="w-cell">{{ Widgets.truncate(modelValue) }}</span>
    <span v-else-if="mode === 'view'" class="w-cell">{{ modelValue }}</span>
    <el-input v-else :model-value="modelValue === undefined || modelValue === null ? '' : modelValue"
        @update:model-value="emitValue($event)" />
</template>
<script>
export default {
    name: 'WText',
    props: {
        modelValue: { default: undefined },
        mode: { type: String, default: 'edit' },      // edit | cell
        field: { type: Object, default: () => ({}) },
        defs: { type: Object, default: () => ({}) },  // 类型定义表（ref 显示名用）
    },
    emits: ['update:modelValue'],
    methods: {
        emitValue(v) { this.$emit('update:modelValue', v) },    },
}
</script>
