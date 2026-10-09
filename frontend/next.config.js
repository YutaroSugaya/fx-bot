/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,
  // Proxy API calls to the Go backend in dev so the browser doesn't need CORS.
  async rewrites() {
    return [
      {
        source: '/api/:path*',
        // 127.0.0.1 を明示 (localhost にしない)。macOS の /etc/hosts は localhost を
        // IPv4(127.0.0.1) と IPv6(::1) の両方に解決し、Node は ::1 を優先して掴むことがあるが
        // バックエンドが ::1 で待ち受けてない瞬間に `ECONNREFUSED ::1:8080` で蹴られる。
        // IPv4 に固定すれば address family のレースが消える。
        destination: `${process.env.NEXT_PUBLIC_API_BASE || 'http://127.0.0.1:8080'}/api/:path*`,
      },
    ]
  },
  // /api/advisor/trigger は Claude CLI を走らせるので 15 分近くかかる。
  // Next の既定の proxy timeout (30 秒) だと "Internal Server Error" が
  // text で返り、ブラウザ側で JSON.parse が落ちる。十分長く取る。
  experimental: {
    proxyTimeout: 20 * 60 * 1000,
  },
}

module.exports = nextConfig
