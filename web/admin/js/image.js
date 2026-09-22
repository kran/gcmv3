// 后台出图的**唯一**一处: 给图片路径拼 imgproc 的缩放参数。
//
// 与小程序端 (association-front/src/utils/image.js) 是同一套口径 —— 两个项目没有共享
// 构建（后台连构建都没有）, 只能各留一份; 参数格式由 gcmv3 的 plugin/imgproc 定义。
//
// 为什么要带参数: 图片都是原图上传（用户手机一张 3~5MB）, 30px 的格子不带参数就会把
// 整张原图下下来再缩 —— 列表一整页几十 MB。非图片（mp4/pdf/…）不拼（拼了会 400）。
window.$img = (function () {
    const IMAGE = /\.(png|jpe?g|gif|webp|bmp|avif|heic|heif|tiff?)$/i

    // 尺寸按"显示尺寸 × 2"取。thumb 必须与 .w-thumb 的 CSS 一致（闸门会核对）。
    const SIZES = {
        thumb: { width: 30, height: 30, mode: 'fill' },     // 列表缩略图
        gallery: { width: 200, height: 200, mode: 'fill' }, // 图集缩略（.gallery-thumb 是 100%）
        preview: { width: 0, height: 120, mode: 'lfit' },   // 编辑态预览（max-height 56px）
    }

    function query(size) {
        if (!size) return ''
        const width = Number(size.width) || 0
        const height = Number(size.height) || 0
        if (width <= 0 && height <= 0) return ''
        // m_fill 必须同时给 w 和 h（只给一边 OSS 会报错）⇒ 缺一边退回 lfit
        const mode = size.mode === 'fill' && width > 0 && height > 0 ? 'fill' : 'lfit'
        const segments = ['image/resize']
        if (width > 0) segments.push('w_' + width)
        if (height > 0) segments.push('h_' + height)
        segments.push('m_' + mode)
        return 'x-oss-process=' + segments.join(',')
    }

    // url 给图片路径加参数; 已有参数不重复拼; 非图片原样返回。
    function url(path, sizeName) {
        if (!path) return path
        const value = String(path)
        if (value.includes('x-oss-process=')) return value
        const size = SIZES[sizeName]
        if (!size) return value
        const bare = value.split('?')[0].split('#')[0]
        if (!IMAGE.test(bare)) return value
        const param = query(size)
        if (!param) return value
        return value + (value.includes('?') ? '&' : '?') + param
    }

    return { url: url, query: query, sizes: SIZES }
})()
