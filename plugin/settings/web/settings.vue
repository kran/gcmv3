<!-- 站点配置面板（插件自带, 自包含 —— 插件面板不引后台的共享组件:s 那些在另一个
     URL 空间下, SFC 的相对 import 解析不到）。
     配置项由站点代码**声明**（类型/标签/说明）, 这里只改值。 -->
<template>
  <div style="padding:20px;max-width:760px;">
    <div style="display:flex;align-items:center;gap:12px;margin-bottom:16px;">
      <span style="font-size:16px;font-weight:600;">站点配置</span>
      <el-button size="small" @click="load">刷新</el-button>
      <span style="color:#999;font-size:12px;">配置项由站点代码声明 —— 这里只能改值</span>
    </div>

    <el-form label-width="130px" v-loading="loading">
      <el-form-item v-for="item in items" :key="item.key" :label="item.label || item.key">
        <el-switch v-if="item.kind === 'bool'" v-model="item.value" />
        <el-input-number v-else-if="item.kind === 'number'" v-model="item.value" />
        <el-select v-else-if="item.kind === 'select'" v-model="item.value" style="width:100%;">
          <el-option v-for="o in item.options || []" :key="o" :label="o" :value="o" />
        </el-select>
        <el-input v-else-if="item.kind === 'textarea' || item.kind === 'richtext' || item.kind === 'json'"
                  v-model="item.text" type="textarea" :rows="item.kind === 'json' ? 5 : (item.kind === 'richtext' ? 6 : 3)" />
        <div v-else-if="item.kind === 'upload-image' || item.kind === 'upload-file'"
             style="display:flex;gap:8px;align-items:center;width:100%;">
          <el-input v-model="item.text" :placeholder="item.kind === 'upload-image' ? '/uploads/xxx.png' : '/uploads/xxx.pdf'" />
          <el-button size="small" @click="pickFile(item)">上传</el-button>
          <img v-if="item.kind === 'upload-image' && item.text" :src="thumbURL(item.text)"
               style="height:40px;border-radius:4px;border:1px solid #eee;" />
        </div>
        <el-input v-else v-model="item.text" />

        <div style="width:100%;margin-top:6px;display:flex;align-items:center;gap:8px;">
          <el-button size="small" type="primary" @click="save(item)">保存</el-button>
          <el-button size="small" @click="clear(item)">清空（回默认）</el-button>
          <span style="color:#bbb;font-size:12px;">
            {{ item.updated_at ? '更新于 ' + fmtTime(item.updated_at) : '默认值' }}
          </span>
          <code style="color:#bbb;font-size:12px;">{{ item.key }}</code>
        </div>
        <div v-if="item.note" style="width:100%;color:#999;font-size:12px;">{{ item.note }}</div>
      </el-form-item>
    </el-form>

    <input type="file" ref="file" style="display:none" @change="onFile" />
  </div>
</template>

<script>
export default {
  data() {
    return { items: [], loading: false, uploading: null }
  },
  mounted() { this.load() },
  methods: {
    async load() {
      this.loading = true
      try {
        const res = await window.$api.get('/admin/settings', {})
        this.items = (res.items || []).map(item => this.toEdit(item))
      } catch (e) {
        ElementPlus.ElMessage.error(e.message || '加载失败')
      } finally {
        this.loading = false
      }
    },
    // toEdit 把服务端的值摊成"编辑用"的两个槽: text（字符串类）与 value（布尔/数字/下拉）
    toEdit(item) {
      const value = item.value
      const out = Object.assign({}, item)
      out.value = value === null || value === undefined ? (item.kind === 'bool' ? false : undefined) : value
      if (item.kind === 'json') {
        out.text = value === null || value === undefined ? '' : JSON.stringify(value, null, 2)
      } else if (item.kind === 'bool' || item.kind === 'number' || item.kind === 'select') {
        out.text = ''
      } else {
        out.text = value === null || value === undefined ? '' : String(value)
      }
      if (item.kind === 'number' && (out.value === undefined || out.value === null)) out.value = 0
      if (item.kind === 'select' && !out.value) out.value = (item.options || [])[0]
      return out
    },
    payload(item) {
      if (item.kind === 'bool' || item.kind === 'number' || item.kind === 'select') return item.value
      if (item.kind === 'json') {
        const text = String(item.text || '').trim()
        if (!text) return null
        try {
          return JSON.parse(text)
        } catch (_) {
          ElementPlus.ElMessage.error('JSON 格式不对')
          throw new Error('bad json')
        }
      }
      return item.text
    },
    async save(item) {
      let value
      try {
        value = this.payload(item)
      } catch (_) {
        return
      }
      try {
        await window.$api.put('/admin/settings/' + encodeURIComponent(item.key), { value })
        ElementPlus.ElMessage.success('已保存')
        this.load()
      } catch (e) {
        ElementPlus.ElMessage.error(e.message || '保存失败')
      }
    },
    async clear(item) {
      try {
        await window.$api.del('/admin/settings/' + encodeURIComponent(item.key))
        ElementPlus.ElMessage.success('已回到默认值')
        this.load()
      } catch (e) {
        ElementPlus.ElMessage.error(e.message || '清空失败')
      }
    },
    pickFile(item) {
      this.uploading = item
      if (this.$refs.file) this.$refs.file.click()
    },
    async onFile(event) {
      const file = event.target.files && event.target.files[0]
      event.target.value = ''
      if (!file || !this.uploading) return
      try {
        const res = await window.$api.upload(file)
        this.uploading.text = res.path
      } catch (_) {}
      this.uploading = null
    },
    thumbURL(path) {
      // 后台的统一出图口（web/admin/js/image.js）: 不给参数就会下整张原图
      if (window.$img) return window.$img.url(path, 'thumb')
      return path
    },
    fmtTime(seconds) {
      const date = new Date(Number(seconds) * 1000)
      if (Number.isNaN(date.getTime())) return ''
      const pad = n => String(n).padStart(2, '0')
      return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`
    },
  },
}
</script>
