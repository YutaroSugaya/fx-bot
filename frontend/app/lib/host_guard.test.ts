import { describe, expect, it } from 'vitest'
import { isAllowedHost, parseAllowedHosts } from './host_guard'

// DNS rebinding 対策: 攻撃者のドメインを 127.0.0.1 に向け直したページから /api を叩かれても、
// Host ヘッダは攻撃者のドメインのままなので、loopback 名と明示した名前以外は拒否する。
describe('isAllowedHost', () => {
  it('allows loopback names with or without a port', () => {
    for (const h of ['localhost', 'localhost:3000', '127.0.0.1:3000', '[::1]:3000', 'LOCALHOST:3000']) {
      expect(isAllowedHost(h, [])).toBe(true)
    }
  })

  it('rejects any other host, including rebinding domains', () => {
    for (const h of ['evil.example:3000', 'attacker.test', '192.168.0.10:3000', 'localhost.evil.example:3000']) {
      expect(isAllowedHost(h, [])).toBe(false)
    }
  })

  it('rejects a missing host header (fail closed)', () => {
    expect(isAllowedHost(null, [])).toBe(false)
    expect(isAllowedHost('', [])).toBe(false)
  })

  it('allows hosts listed explicitly (port ignored, case-insensitive)', () => {
    expect(isAllowedHost('dash.example.com:3000', ['dash.example.com'])).toBe(true)
    expect(isAllowedHost('Dash.Example.com', ['dash.example.com'])).toBe(true)
    expect(isAllowedHost('other.example.com', ['dash.example.com'])).toBe(false)
  })
})

describe('parseAllowedHosts', () => {
  it('splits a comma separated env value and drops blanks / ports', () => {
    expect(parseAllowedHosts(' dash.example.com:3000, ,my-mac.local ')).toEqual(['dash.example.com', 'my-mac.local'])
    expect(parseAllowedHosts(undefined)).toEqual([])
  })
})
