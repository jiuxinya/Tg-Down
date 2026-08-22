import { writeFileSync } from 'node:fs'
import { defineConfig } from 'vite'
import preact from '@preact/preset-vite'

const embedPlaceholder = 'Keeps the go:embed directory present in clean checkouts.\n'

// 产物直接落进 internal/web/static/dist，由 go:embed 打进单二进制。
// 单文件输出（无 code split）：管理台总共就这么点代码，多一个 chunk 只是多一次请求，
// 而且 CSP 下 modulepreload 反而更麻烦。
export default defineConfig({
  plugins: [
    preact(),
    {
      name: 'preserve-go-embed-placeholder',
      apply: 'build',
      closeBundle() {
        // emptyOutDir 会先清空目标目录；构建结束后恢复被 Git 跟踪的占位文件。
        writeFileSync(new URL('../internal/web/static/dist/.gitkeep', import.meta.url), embedPlaceholder)
      },
    },
  ],
  base: './',
  build: {
    outDir: '../internal/web/static/dist',
    emptyOutDir: true,
    target: 'es2020',
    rollupOptions: {
      output: {
        manualChunks: undefined,
        entryFileNames: 'assets/[name]-[hash].js',
        chunkFileNames: 'assets/[name]-[hash].js',
        assetFileNames: 'assets/[name]-[hash][extname]',
      },
    },
  },
  server: {
    // 开发时把 API 代理到本地 Go 服务，前端可用 npm run dev 热更新
    proxy: {
      '/api': 'http://127.0.0.1:8080',
    },
  },
})
