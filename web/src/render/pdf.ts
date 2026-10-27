// PDF via pdf.js: scripts in PDFs are off, the worker comes from the app's origin.
import * as pdfjs from 'pdfjs-dist'
import workerUrl from 'pdfjs-dist/build/pdf.worker.min.mjs?url'

pdfjs.GlobalWorkerOptions.workerSrc = workerUrl

export async function renderPdf(url: string, root: HTMLElement, isCancelled: () => boolean) {
  const doc = await pdfjs.getDocument({ url, enableXfa: false, withCredentials: false } as any).promise
  const width = Math.max(320, root.clientWidth - 16)
  for (let i = 1; i <= doc.numPages && i <= 200; i++) {
    if (isCancelled()) return
    const page = await doc.getPage(i)
    const base = page.getViewport({ scale: 1 })
    const scale = width / base.width
    const vp = page.getViewport({ scale: scale * (window.devicePixelRatio || 1) })
    const canvas = document.createElement('canvas')
    canvas.width = vp.width
    canvas.height = vp.height
    canvas.style.width = `${width}px`
    canvas.className = 'pdf-page'
    root.appendChild(canvas)
    await page.render({ canvas, viewport: vp } as any).promise
  }
}
