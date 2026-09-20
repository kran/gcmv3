<!-- kind multiselect：多选枚举（选项来自字段声明）；值 = 选项字符串数组。
     编辑 = 平铺 checkbox（一眼看全"这个人有哪些角色"，比多选下拉安全 —— 下拉会把未选项藏起来）；
     只读 = 标签列表（不是打钩的框）。选项太多属于站点的配置责任（该改用 refs）。
     文件名就是 kind 名（web/admin/widgets/multiselect.vue），mode = edit 编辑 / cell 列表单元格 / view 只读详情（后两者都只展示值, 不出现控件）。 -->
<template>
    <span v-if="mode !== 'edit'" class="w-cell">{{ labels || '—' }}</span>
    <span v-else class="w-multiselect">
        <el-checkbox-group :model-value="current" @update:model-value="emitValue">
            <el-checkbox v-for="option in options" :key="option" :label="option">{{ option }}</el-checkbox>
        </el-checkbox-group>
    </span>
</template>
<script>
export default {
    name: 'WMultiselect',
    props: {
        modelValue: { default: undefined },
        mode: { type: String, default: 'edit' },      // edit | cell
        field: { type: Object, default: () => ({}) },
        defs: { type: Object, default: () => ({}) },
    },
    emits: ['update:modelValue'],
    computed: {
        options() { return (this.field && this.field.options) || [] },
        current() { return Array.isArray(this.modelValue) ? this.modelValue : [] },
        // 只读显示**全部**已选值 —— 不在选项里的历史值也要显示（词表移除后不能静默隐藏；
        // 存量体检靠 tools/roles-check）
        labels() { return this.current.filter(Boolean).join('、') },
    },
    methods: {
        emitValue(v) { this.$emit('update:modelValue', v) },
    },
}
</script>
<style>
/* 多选：换行排布（选项多时多列） */
.w-multiselect { display: block; }
.w-multiselect .el-checkbox { margin-right: 16px; }
</style>
