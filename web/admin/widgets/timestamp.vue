<!-- kind timestamp：时间点；库里/接口是 **Unix 秒**（整数, UTC 绝对时刻），界面按设备本地时间。
     文件名就是 kind 名（web/admin/widgets/timestamp.vue），mode="edit" 编辑 / mode="cell" 只读。 -->
<template>
    <span v-if="mode === 'cell'" class="w-cell">{{ modelValue ? Widgets.localTime(modelValue) : '' }}</span>
    <span v-else class="w-timestamp">
        <el-date-picker type="datetime" :model-value="toDate(modelValue)" :clearable="true"
            placeholder="选择时间" style="width:230px;"
            @update:model-value="emitValue($event)" />
        <!-- 值不是整数秒（旧库的 ISO 字符串、毫秒、垃圾）→ 选择器读不出来: 提示出来,
             不能静默清空（那是数据丢失, 也会把"没改"判成"改了"）。 -->
        <span v-if="!parseable" class="w-timestamp-warn">值不是整数秒：{{ modelValue }}（时间字段是 Unix 秒, 毫秒要除 1000）</span>
    </span>
</template>
<script>
// 值必须是**整数秒**（Unix 秒）。毫秒（JS 的 Date.now()）超界 ⇒ 选择器读不出来,
// 提示用户 —— 静默当毫秒会显示成 1970 年, 静默不写又会把"没改"判成"改了"。
function parseable(v) {
    if (v === undefined || v === null || v === '') return true
    if (typeof v !== 'number' || !Number.isInteger(v)) return false
    return v > 0 && v <= MAX_UNIX_SECONDS
}
const MAX_UNIX_SECONDS = 100000000000

export default {
    name: 'WDatetime',
    props: {
        modelValue: { default: undefined },
        mode: { type: String, default: 'edit' },      // edit | cell
        field: { type: Object, default: () => ({}) },
        defs: { type: Object, default: () => ({}) },  // 类型定义表（ref 显示名用）
    },
    emits: ['update:modelValue'],
    computed: {
        parseable() { return parseable(this.modelValue) },
    },
    methods: {
        emitValue(raw) {
            const v = this.toUnix(raw)
            if (v === null && !this.parseable) return
            // 选择器解析不了库里的值（emit null）时不要回写：留着原值, 提示用户迁移。
            if (v === this.modelValue) return
            this.$emit('update:modelValue', v)
        },
        // Unix 秒 ↔ Date（JS 的 Date 是毫秒 ⇒ 两边差了 1000）
        toDate(v) { return parseable(v) && v ? new Date(v * 1000) : null },
        toUnix(v) { return v ? Math.floor(new Date(v).getTime() / 1000) : null },
    },
}
</script>
<style>
/* 编辑态：选择器 + 旧值提示 */
.w-timestamp { display: inline-flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.w-timestamp-warn { font-size: 12px; color: var(--el-color-warning); }
</style>
