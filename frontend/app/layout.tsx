export const metadata = {
  title: 'fx-bot ダッシュボード',
  description: 'GMO コイン外国為替FX API 向けの非公式な自動売買 bot のダッシュボード',
}

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="ja">
      <body
        style={{
          fontFamily:
            'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", "Courier New", monospace',
          background: '#0f1115',
          color: '#e6e9ef',
          margin: 0,
          padding: '24px',
          minHeight: '100vh',
        }}
      >
        {children}
      </body>
    </html>
  )
}
