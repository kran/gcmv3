<!-- kind richtext：富文本。
     文件名就是 kind 名（web/admin/widgets/richtext.vue）。
     mode = edit 编辑器 / cell 列表单元格（去标签截断）/ view 只读详情（**渲染成 HTML**, 看全）。 -->
<template>
    <span v-if="mode === 'cell'" class="w-cell">{{ Widgets.truncate(Widgets.plain(modelValue)) }}</span>
    <div v-else-if="mode === 'view'" class="w-cell w-richtext" v-html="modelValue || ''"></div>
    <rich-editor v-else :model-value="modelValue || ''" @update:model-value="emitValue($event)" />
</template>
<script>
export default {
    name: 'WRichtext',
    props: {
        modelValue: { default: undefined },
        mode: { type: String, default: 'edit' },      // edit | cell | view
        field: { type: Object, default: () => ({}) },
        defs: { type: Object, default: () => ({}) },  // 类型定义表（ref 显示名用）
    },
    emits: ['update:modelValue'],
    components: { RichEditor: Vue.defineAsyncComponent(() => window.Panel.loadComponent('pages/RichEditor.vue')) },
    methods: {
        emitValue(v) { this.$emit('update:modelValue', v) },
    },
}
</script>
<style>
/* 只读详情（view）: 把富文本按块级排版显示出来（只读, 不是编辑器）。
   内容在写路径上已经过 bluemonday 白名单（会员提交的 body 一律消毒）。 */
.w-richtext { display: block; max-height: 420px; overflow: auto; }
.w-richtext p { margin: 0 0 .5em; }
.w-richtext img { max-width: 100%; }
</style>
