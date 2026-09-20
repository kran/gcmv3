<!-- kind number：数字；列表里右对齐。
     文件名就是 kind 名（web/admin/widgets/number.vue），mode = edit 编辑 / cell 列表单元格 / view 只读详情（后两者都只展示值, 不出现控件）。 -->
<template>
    <span v-if="mode !== 'edit'" class="w-cell w-num">{{ modelValue === undefined || modelValue === null ? '' : modelValue }}</span>
    <el-input-number v-else :model-value="modelValue" @update:model-value="emitValue($event)" style="width:200px;" />
</template>
<script>
export default {
    name: 'WNumber',
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
<style>
/* 数字：列表里右对齐 */
.w-num { justify-content: flex-end; }
</style>
