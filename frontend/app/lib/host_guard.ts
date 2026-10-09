// Host ヘッダの検査(DNS rebinding 対策)。ダッシュボードは既定で 127.0.0.1 にだけ bind するが、
// 攻撃者のドメインを 127.0.0.1 に解決させたページは、ブラウザから見て「同じ origin」として /api を叩ける。
// そのときも Host ヘッダは攻撃者のドメインのままなので、loopback 名と明示した名前以外は拒否する。

const LOOPBACK_HOSTS = ['localhost', '127.0.0.1', '::1']

// hostnameOf は "name:port" / "[v6]:port" から名前だけを小文字で返す。
function hostnameOf(host: string): string {
  const h = host.trim().toLowerCase()
  if (h.startsWith('[')) {
    const end = h.indexOf(']')
    return end > 0 ? h.slice(1, end) : h
  }
  const colon = h.lastIndexOf(':')
  return colon >= 0 ? h.slice(0, colon) : h
}

// parseAllowedHosts は DASHBOARD_ALLOWED_HOSTS(カンマ区切り)を名前の配列にする。ポートは無視する。
export function parseAllowedHosts(raw: string | undefined): string[] {
  return (raw ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter((s) => s !== '')
    .map(hostnameOf)
}

// isAllowedHost は Host ヘッダが loopback 名か、明示的に許可した名前なら true。無ければ拒否(fail-close)。
export function isAllowedHost(host: string | null | undefined, extra: string[]): boolean {
  if (!host) return false
  const name = hostnameOf(host)
  return LOOPBACK_HOSTS.includes(name) || extra.includes(name)
}
