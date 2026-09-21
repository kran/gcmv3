<!-- kind upload-image：单图；编辑 = 路径 + 上传，列表 = 缩略图。
     文件名就是 kind 名（web/admin/widgets/upload-image.vue），mode = edit 编辑 / cell 列表单元格 / view 只读详情（后两者都只展示值, 不出现控件）。 -->
<template>
    <span v-if="mode !== 'edit'" class="w-cell">
        <img v-if="modelValue" :src="thumbSrc" class="w-thumb" />
        <span v-else class="w-empty">—</span>
    </span>
    <div v-else class="w-image">
        <el-input :model-value="modelValue" @update:model-value="emitValue($event)" placeholder="/uploads/xxx.png" />
        <input type="file" style="display:none;" accept="image/*" ref="file" @change="upload" />
        <el-button size="small" @click="pick">上传</el-button>
        <img v-if="modelValue" :src="modelValue" class="w-image-preview" />
    </div>
</template>
<script>
// 缩略图参数: 与 .w-thumb 的 30×30 对应（改尺寸要同时改样式, 闸门会核对两处一致）。
// 为什么要带参数: 30px 的格子如果不带, 浏览器会**下整张原图**再缩 —— 列表一整页几十 MB。
// 口径与 plugin/imgproc 一致（m_fill 必须给 w 和 h）; 本地部署时 imgproc 的本地 hook
// 也认这个参数 ⇒ 一样先缩再传。
const THUMB_PROCESS = 'x-oss-process=image/resize,w_30,h_30,m_fill'

// withThumb 已经有参数就不重复拼（站点可能自己给了别的处理参数）。
function withThumb(value) {
    if (!value) return value
    const url = String(value)
    if (url.includes('x-oss-process=')) return url
    return url + (url.includes('?') ? '&' : '?') + THUMB_PROCESS
}

export default {
    name: 'WImage',
    props: {
        modelValue: { default: undefined },
        mode: { type: String, default: 'edit' },      // edit | cell
        field: { type: Object, default: () => ({}) },
        defs: { type: Object, default: () => ({}) },  // 类型定义表（ref 显示名用）
    },
    emits: ['update:modelValue'],
    computed: {
        // cell 与 view 都用 .w-thumb（同一个 30×30 的格子）⇒ 都走缩放后的 URL
        thumbSrc() { return withThumb(this.modelValue) },
    },
    methods: {
        emitValue(v) { this.$emit('update:modelValue', v) },
        pick() { if (this.$refs.file) this.$refs.file.click() },
        async upload(ev) {
            const file = ev.target.files && ev.target.files[0]
            if (!file) return
            try {
                const res = await window.$api.upload(file)
                this.emitValue(res.path)
                ElMessage.success('已上传')
            } catch (_) {}
            ev.target.value = ''
        },
    },
}
</script>
<style>
.w-image { display: flex; gap: 8px; align-items: center; width: 100%; }
.w-image-preview { max-height: 56px; border-radius: 3px; border: 1px solid #eee; }
.w-thumb { width: 30px; height: 30px; object-fit: cover; border-radius: 3px; border: 1px solid #eee; }
</style>
