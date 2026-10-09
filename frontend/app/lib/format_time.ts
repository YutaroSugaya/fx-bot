// page.tsx 等で共有する日時フォーマッタ。
// fmtJST は ISO 文字列を Asia/Tokyo に固定整形して "YYYY/MM/DD HH:mm:ss JST" を返す。
// 入力が空文字 / parse 不能なら原文を返す (UI が崩れないようにするため)。

const jstFormatter = new Intl.DateTimeFormat('ja-JP', {
  timeZone: 'Asia/Tokyo',
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false,
})

export function fmtJST(iso: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (isNaN(d.getTime())) return iso
  return `${jstFormatter.format(d)} JST`
}
