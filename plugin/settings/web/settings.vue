<!-- 站点配置面板（插件自带, 自包含 —— 插件面板引后台组件只能用
     Panel.loadComponent('pages/x.vue') 这种走文档基址的相对地址, SFC 的相对 import
     解析不到）。

     形状就是 v2 那套: 表格 + 分组筛选 + 弹窗增删改。**库是真相** —— 键可以随便建,
     类型以库里的列为准; 预置声明（Label/Note/Options/Default）只用来给标签和默认值。

     __PREFIX__ 由 Go 侧替换成实际端点前缀（Options.Prefix 改了也不会打错地址）。 -->
<template>
  <div style="padding:20px;max-width:1100px;">
    <div style="display:flex;gap:8px;align-items:center;margin-bottom:14px;">
      <span style="font-size:16px;font-weight:600;">站点配置</span>
      <el-select v-model="group" placeholder="分组" size="small" style="width:160px" clearable>
        <el-option v-for="g in groups" :key="g" :label="g || '(未分组)'" :value="g" />
      </el-select>
      <el-button size="small" @click="refresh">刷新</el-button>
      <el-button type="primary" size="small" @click="openEdit(null)">新建</el-button>
      <span style="color:#999;font-size:12px;">类型以库里的为准 —— 这里可以建任意键</span>
    </div>

    <el-table :data="shown" v-loading="loading">
      <el-table-column prop="key" label="key" width="220">
        <template #default="{ row: r }">
          <code>{{ r.key }}</code>
          <div v-if="r.label" style="color:#999;font-size:12px;">{{ r.label }}</div>
        </template>
      </el-table-column>
      <el-table-column prop="group" label="分组" width="110" />
      <el-table-column label="类型" width="110">
        <template #default="{ row: r }"><el-tag size="small">{{ r.type }}</el-tag></template>
      </el-table-column>
      <el-table-column label="value" min-width="240" show-overflow-tooltip>
        <template #default="{ row: r }">
          <img v-if="r.type === 'upload-image' && r.value" :src="thumbURL(r.value)"
               style="height:30px;border-radius:3px;border:1px solid #eee;" />
          <code v-else style="font-size:12px;">{{ valueOf(r) }}</code>
        </template>
      </el-table-column>
      <el-table-column label="更新时间" width="150">
        <template #default="{ row: r }">{{ fmtTime(r.updated_at) }}</template>
      </el-table-column>
      <el-table-column label="操作" width="130" fixed="right">
        <template #default="{ row: r }">
          <el-button link size="small" @click="openEdit(r)">编辑</el-button>
          <el-button link size="small" @click="doDelete(r)">删除</el-button>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog append-to-body v-model="dialog.visible"
               :title="dialog.isNew ? '新建配置' : '编辑 ' + dialog.key" width="60vw">
      <el-form label-width="70px">
        <el-form-item label="key">
          <el-input v-model="dialog.key" :disabled="!dialog.isNew" placeholder="footer / seo.default" />
        </el-form-item>
        <el-form-item label="分组">
          <el-input v-model="dialog.group" placeholder="site / seo / list" />
        </el-form-item>
        <el-form-item label="类型">
          <el-radio-group v-model="dialog.kind">
            <el-radio v-for="k in kinds" :key="k" :value="k" size="small">{{ k }}</el-radio>
          </el-radio-group>
        </el-form-item>

        <!-- 按类型渲染控件: 单选里那 8 种 + 库里可能出现的 select / upload-image / json -->
        <el-form-item v-if="dialog.kind === 'text'" label="value">
          <el-input v-model="dialog.scalar" placeholder="字符串值" />
        </el-form-item>
        <el-form-item v-else-if="dialog.kind === 'textarea'" label="value">
          <el-input v-model="dialog.scalar" type="textarea" :rows="4" />
        </el-form-item>
        <el-form-item v-else-if="dialog.kind === 'richtext'" label="value">
          <!-- 后台的富文本编辑器（不是多行文本框 —— 富文本手写标签太容易坏） -->
          <rich-editor :model-value="dialog.scalar || ''" style="width:100%;"
                       @update:model-value="dialog.scalar = $event" />
        </el-form-item>
        <el-form-item v-else-if="dialog.kind === 'number'" label="value">
          <el-input-number v-model="dialog.scalar" style="width:200px;" />
        </el-form-item>
        <el-form-item v-else-if="dialog.kind === 'bool'" label="value">
          <el-switch v-model="dialog.scalar" />
        </el-form-item>
        <el-form-item v-else-if="dialog.kind === 'select'" label="value">
          <el-select v-if="options.length" v-model="dialog.scalar" style="width:240px;">
            <el-option v-for="o in options" :key="o" :label="o" :value="o" />
          </el-select>
          <!-- 没有预置候选项的 select: 只能手填（候选项是多选一的全部可能值, 编不出来） -->
          <el-input v-else v-model="dialog.scalar" placeholder="候选项由站点代码声明（这里手填一个值）" />
        </el-form-item>
        <el-form-item v-else-if="dialog.kind === 'upload-image' || dialog.kind === 'upload-file'" label="value">
          <div style="display:flex;gap:8px;align-items:center;width:100%;">
            <el-input v-model="dialog.scalar"
                      :placeholder="dialog.kind === 'upload-image' ? '/uploads/xxx.png' : '/uploads/xxx.pdf'" />
            <el-button size="small" @click="pickFile">上传</el-button>
            <img v-if="dialog.kind === 'upload-image' && dialog.scalar" :src="thumbURL(dialog.scalar)"
                 style="height:40px;border-radius:4px;border:1px solid #eee;" />
          </div>
        </el-form-item>
        <el-form-item v-else-if="dialog.kind === 'object'" label="字段">
          <div style="width:100%;">
            <div v-for="(kv, i) in dialog.entries" :key="i" style="display:flex;gap:8px;margin-bottom:8px;">
              <el-input v-model="kv.name" placeholder="键" style="width:160px;" />
              <el-input v-model="kv.value" placeholder="值 (JSON 或字符串)" style="flex:1;" />
              <el-button link @click="dialog.entries.splice(i, 1)">删</el-button>
            </div>
            <el-button size="small" @click="dialog.entries.push({ name: '', value: '' })">+ 添加字段</el-button>
          </div>
        </el-form-item>
        <el-form-item v-else-if="dialog.kind === 'array'" label="元素">
          <div style="width:100%;">
            <div v-for="(v, i) in dialog.array" :key="i" style="display:flex;gap:8px;margin-bottom:8px;">
              <el-input v-model="dialog.array[i]" placeholder="元素 (JSON 或字符串)" style="flex:1;" />
              <el-button link @click="dialog.array.splice(i, 1)">删</el-button>
            </div>
            <el-button size="small" @click="dialog.array.push('')">+ 添加元素</el-button>
          </div>
        </el-form-item>
        <el-form-item v-else-if="dialog.kind === 'json'" label="value">
          <el-input v-model="dialog.text" type="textarea" :rows="6"
                    placeholder='{"a": 1} 或 ["x", "y"] —— 任意 JSON' />
        </el-form-item>

        <div v-if="dialog.note" style="color:#999;font-size:12px;margin-left:70px;">{{ dialog.note }}</div>
      </el-form>
      <template #footer>
        <el-button @click="dialog.visible = false">取消</el-button>
        <el-button type="primary" :loading="dialog.saving" @click="doSave">保存</el-button>
      </template>
    </el-dialog>

    <input ref="fileInput" type="file" style="display:none;" @change="onFile" />
  </div>
</template>
<script>
// 端点的实际前缀（Go 侧注入 —— Options.Prefix 改了这里跟着改）
const PREFIX = '__PREFIX__'

// 类型清单就是 v2 表单里的那 8 个单选（**与 Go 侧无关** —— 服务端不校验类型,
// 这里只是"选个控件来编这个值"。库里出现了清单外的类型也能编辑（下面按类型渲染的
// 分支里还有 select / upload-image / json 三种, 只是不进这个单选）。
const KINDS = ['text', 'textarea', 'number', 'bool', 'object', 'array', 'upload-file', 'richtext']

export default {
  name: 'SettingsPanel',
  // rich 用后台的编辑器（相对地址走文档基址 /admin/ui/ —— 与 widgets/richtext.vue 同一手法）
  components: {
    RichEditor: Vue.defineAsyncComponent(() => window.Panel.loadComponent('pages/RichEditor.vue')),
  },
  data() {
    return {
      rows: [], loading: false, group: '',
      kinds: KINDS,
      dialog: {
        visible: false, saving: false, isNew: true, key: '', group: '', kind: 'text',
        note: '', options: [], scalar: null, text: '', entries: [], array: [],
      },
    }
  },
  computed: {
    groups() {
      const seen = {}
      this.rows.forEach(r => { seen[r.group || ''] = true })
      return Object.keys(seen).sort()
    },
    // 分组筛选在**前端**做: 服务端筛过的话, 分组下拉里的其它分组会跟着消失（v2 就这么坏的）
    shown() {
      if (!this.group) return this.rows
      return this.rows.filter(r => (r.group || '') === this.group)
    },
    options() { return this.dialog.options || [] },
  },
  mounted() { this.refresh() },
  methods: {
    async refresh() {
      this.loading = true
      try {
        const res = await window.$api.get(PREFIX)
        this.rows = res.items || []
      } catch (e) {
        ElementPlus.ElMessage.error(e.message || '加载失败')
      } finally {
        this.loading = false
      }
    },
    valueOf(r) {
      if (r.value === null || r.value === undefined) return '—'
      const text = JSON.stringify(r.value)
      return text.length > 40 ? text.slice(0, 40) + '…' : text
    },
    fmtTime(seconds) {
      if (!seconds) return '—'   // 0 = 没写过（值来自预置的默认值）
      const date = new Date(Number(seconds) * 1000)
      if (Number.isNaN(date.getTime())) return ''
      const pad = n => String(n).padStart(2, '0')
      return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`
    },
    openEdit(r) {
      const d = this.dialog
      d.isNew = !r
      d.key = r ? r.key : ''
      d.group = r && r.group ? r.group : ''
      // 类型**以库里的为准**（字符串类配置反推不出 text/textarea/upload-file 的区别）
      d.kind = r && r.type ? r.type : 'text'
      d.note = r ? (r.note || '') : ''
      d.options = (r && r.options) || []
      const value = r ? r.value : null
      d.scalar = null
      d.text = ''
      d.entries = []
      d.array = []
      if (d.kind === 'bool') d.scalar = !!value
      else if (d.kind === 'number') d.scalar = typeof value === 'number' ? value : Number(value) || 0
      else if (d.kind === 'object') {
        d.entries = Object.entries(value || {}).map(kv => ({
          name: kv[0], value: typeof kv[1] === 'string' ? kv[1] : JSON.stringify(kv[1]),
        }))
      } else if (d.kind === 'array') {
        d.array = (value || []).map(x => typeof x === 'string' ? x : JSON.stringify(x))
      } else if (d.kind === 'json') {
        d.text = value === null || value === undefined ? '' : JSON.stringify(value, null, 2)
      } else {
        // 字符串类的槽: 值不是字符串就原样 JSON 出来（类型与值本来就不保证对得上 ——
        // v2 不管这个, 但至少不能把值显示丢）
        d.scalar = value === null || value === undefined ? '' :
          (typeof value === 'string' ? value : JSON.stringify(value))
      }
      if (d.kind === 'select' && !d.scalar && d.options.length) d.scalar = d.options[0]
      d.visible = true
    },
    // 编辑形态 → 提交的值（形态不对就在这里拦住, 不把坏值写进库）
    payload() {
      const d = this.dialog
      switch (d.kind) {
        case 'bool': return !!d.scalar
        case 'number': return Number(d.scalar) || 0
        case 'object': {
          const out = {}
          d.entries.forEach(kv => { if (kv.name) out[kv.name] = this.parseVal(kv.value) })
          return out
        }
        case 'array': return d.array.map(this.parseVal).filter(x => x !== '')
        case 'json': {
          const text = String(d.text || '').trim()
          if (!text) return null
          try { return JSON.parse(text) } catch (_) {
            ElementPlus.ElMessage.error('JSON 格式不对')
            throw new Error('bad json')
          }
        }
        default: return d.scalar === null || d.scalar === undefined ? '' : String(d.scalar)
      }
    },
    async doSave() {
      let value
      try {
        value = this.payload()
      } catch (_) {
        return
      }
      const d = this.dialog
      d.saving = true
      try {
        await window.$api.post(PREFIX, { key: d.key, group: d.group, type: d.kind, value })
        ElementPlus.ElMessage.success('已保存')
        d.visible = false
        this.refresh()
      } catch (_) { /* api.js 已提示 */ }
      d.saving = false
    },
    async doDelete(r) {
      try {
        await ElementPlus.ElMessageBox.confirm('删除配置 ' + r.key + ' ？（预置项会回到声明里的默认值）', '确认', { type: 'warning' })
      } catch (_) {
        return // 取消
      }
      try {
        await window.$api.del(PREFIX + '/' + encodeURIComponent(r.key))
        ElementPlus.ElMessage.success('已删除')
        this.refresh()
      } catch (_) { /* api.js 已提示 */ }
    },
    pickFile() { if (this.$refs.fileInput) this.$refs.fileInput.click() },
    async onFile(ev) {
      const file = ev.target.files && ev.target.files[0]
      ev.target.value = ''
      if (!file) return
      try {
        const res = await window.$api.upload(file)
        this.dialog.scalar = res.path
        ElementPlus.ElMessage.success('已上传')
      } catch (_) { /* api.js 已提示 */ }
    },
    thumbURL(path) {
      // 后台统一的出图口（js/image.js）: 不给参数就下整张原图
      if (window.$img) return window.$img.url(path, 'thumb')
      return path
    },
    // 结构化编辑器里的一格: 值先当 JSON 试, 不是就当字符串（"3" 与 3 由写的人决定）
    parseVal(raw) {
      const text = String(raw).trim()
      if (text === '') return ''
      try { return JSON.parse(text) } catch (_) { return text }
    },
  },
}
</script>
