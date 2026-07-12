// Vendors the VAD model/runtime assets into public/ so Vite serves them as
// plain static files (both in dev and in the embedded Go build). Sourced from
// node_modules rather than committed to git since they're multi-MB binaries.
import { copyFileSync, mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = dirname(dirname(fileURLToPath(import.meta.url)))
const destDir = join(root, 'public', 'vad')
mkdirSync(destDir, { recursive: true })

const files = [
  'node_modules/@ricky0123/vad-web/dist/silero_vad_v5.onnx',
  'node_modules/@ricky0123/vad-web/dist/vad.worklet.bundle.min.js',
  'node_modules/onnxruntime-web/dist/ort-wasm-simd-threaded.wasm',
  // Companion JS loader the wasm backend dynamically imports at runtime.
  'node_modules/onnxruntime-web/dist/ort-wasm-simd-threaded.mjs',
]

for (const src of files) {
  copyFileSync(join(root, src), join(destDir, src.split('/').pop()))
}
