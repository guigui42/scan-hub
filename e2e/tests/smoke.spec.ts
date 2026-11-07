import { expect, test, type Page } from '@playwright/test'

// Every page must run under the strict CSP without violations or JS errors.
function watchErrors(page: Page): string[] {
  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`))
  page.on('console', (m) => {
    if (m.type() === 'error') errors.push(`console: ${m.text()}`)
  })
  return errors
}

async function newSession(page: Page, kind: 'Feeder' | 'Glass'): Promise<string> {
  await page.goto('/')
  await page.locator('.ticket', { hasText: kind }).click()
  await page.waitForURL(/\/sessions\/[0-9-a-f]+/)
  return new URL(page.url()).pathname
}

test('desk renders device cards without CSP or JS errors', async ({ page }) => {
  const errors = watchErrors(page)
  await page.goto('/')
  await expect(page.locator('h1')).toContainText('filing')
  await expect(page.locator('#status .card')).toHaveCount(3)
  await expect(page.locator('#status .stamp').first()).toHaveText(/Ready|Connecting/)
  expect(errors).toEqual([])
})

test('feeder scan runs as a polled job and pages appear without reload', async ({ page }) => {
  const errors = watchErrors(page)
  await newSession(page, 'Feeder')
  await page.getByRole('button', { name: 'Feed the stack' }).click()
  await expect(page.locator('#job')).toContainText('pages added', { timeout: 15_000 })
  await expect(page.locator('#pages .sheet')).toHaveCount(6)
  await expect(page.locator('#pages .count')).toHaveText('6')
  expect(errors).toEqual([])
})

test('double-clicking Scan starts a single job', async ({ page }) => {
  await newSession(page, 'Feeder')
  const scan = page.getByRole('button', { name: 'Feed the stack' })
  await scan.dblclick()
  await expect(page.locator('#job')).toContainText('pages added', { timeout: 15_000 })
  await expect(page.locator('#pages .sheet')).toHaveCount(6)
  await expect(page.locator('#flash')).toBeEmpty()
})

test('glass scan from the switch adds one page', async ({ page }) => {
  await newSession(page, 'Glass')
  await expect(page.getByRole('button', { name: 'Scan the glass' })).toBeVisible()
  await page.getByRole('button', { name: 'Scan the glass' }).click()
  await expect(page.locator('#job')).toContainText('1 page added', { timeout: 15_000 })
  await expect(page.locator('#pages .sheet')).toHaveCount(1)
  await page.getByRole('button', { name: /Feeder/ }).click()
  await expect(page.getByRole('button', { name: 'Feed the stack' })).toBeVisible()
})

test('page delete needs a second tap', async ({ page }) => {
  await newSession(page, 'Glass')
  await page.getByRole('button', { name: 'Scan the glass' }).click()
  await expect(page.locator('#pages .sheet')).toHaveCount(1, { timeout: 15_000 })
  await page.getByRole('button', { name: 'Remove page 1', exact: true }).click()
  await expect(page.locator('#pages .sheet')).toHaveCount(1) // armed, not deleted
  await page.getByRole('button', { name: 'Confirm: remove page 1' }).click()
  await expect(page.locator('#pages .sheet')).toHaveCount(0)
  await expect(page.locator('#pages')).toContainText('Nothing on the desk yet')
})

test('server errors are swapped into the flash area', async ({ page, request }) => {
  const path = await newSession(page, 'Glass')
  // Discard the session behind the page's back, then try to scan into it.
  const token = await page.evaluate(() =>
    JSON.parse(document.body.getAttribute('hx-headers:inherited') || '{}')['X-CSRF-Token'],
  )
  const cookies = await page.context().cookies()
  const resp = await request.delete(path, {
    headers: {
      'X-CSRF-Token': token,
      Origin: 'http://127.0.0.1:8098',
      'HX-Request': 'true',
      Cookie: cookies.map((c) => `${c.name}=${c.value}`).join('; '),
    },
  })
  expect(resp.status()).toBe(204)
  await page.getByRole('button', { name: 'Scan the glass' }).click()
  await expect(page.locator('#flash .flash')).toContainText('no longer exists')
  await page.locator('#flash .flash-x').click()
  await expect(page.locator('#flash')).toBeEmpty()
})

test('send goes to the outbox, which polls until filed; back and refresh behave', async ({ page }, info) => {
  // Projects share one demo server: keep titles unique.
  const title = `E2E invoice ${info.project.name}-${Date.now()}`
  const path = await newSession(page, 'Glass')
  await page.locator('#title').fill(title)
  await page.locator('#title').press('Enter')
  await expect(page.locator('.title-state')).toContainText('Saved')
  await page.getByRole('button', { name: 'Scan the glass' }).click()
  await expect(page.locator('#pages .sheet')).toHaveCount(1, { timeout: 15_000 })
  await expect(page.getByRole('link', { name: 'Download PDF' })).toHaveAttribute('href', `${path}/pdf`)

  await page.getByRole('button', { name: /Send to Paperless/ }).click()
  await page.waitForURL('**/outbox')
  const entry = page.locator('.entry', { hasText: title })
  await expect(entry).toBeVisible()
  await expect(entry.locator('.stamp')).toContainText('Filed', { timeout: 30_000 })

  await page.reload()
  await expect(page.locator('.entry', { hasText: title }).locator('.stamp')).toContainText('Filed')
  await page.goBack()
  // The session was handed off, so its page now explains that.
  await expect(page.locator('h1')).toHaveText('Not found')
  await expect(page.locator('.lede')).toContainText('no longer exists')
})

test('layout fits a phone screen', async ({ page }, info) => {
  test.skip(info.project.name !== 'mobile', 'mobile only')
  await newSession(page, 'Feeder')
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
  expect(overflow).toBeLessThanOrEqual(0)
})
