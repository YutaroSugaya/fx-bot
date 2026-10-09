import { NextResponse } from 'next/server'
import type { NextRequest } from 'next/server'
import { isAllowedHost, parseAllowedHosts } from './app/lib/host_guard'

// DNS rebinding 対策: Host ヘッダが loopback 名(localhost / 127.0.0.1 / [::1])か
// DASHBOARD_ALLOWED_HOSTS(カンマ区切り)に無いリクエストは、ページも /api の proxy も 403 で返す。
// 別のホスト名でダッシュボードを開くなら DASHBOARD_ALLOWED_HOSTS にその名前を入れる。
export function proxy(request: NextRequest) {
  const allowed = parseAllowedHosts(process.env.DASHBOARD_ALLOWED_HOSTS)
  if (!isAllowedHost(request.headers.get('host'), allowed)) {
    return new NextResponse('forbidden host', { status: 403 })
  }
  return NextResponse.next()
}
