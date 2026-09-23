// The page shows what the stream says and sends back what is typed. Nothing
// here knows what a command is or what a choice means: the words are the
// session's, and what is picked goes back as it was given.

const chat = document.getElementById('chat')
const composer = document.getElementById('composer')
const typed = document.getElementById('text')
const picked = document.getElementById('pick')
const pictures = document.getElementById('pictures')
const quick = document.getElementById('quick')
const writing = document.getElementById('writing')
const note = document.getElementById('note')
const stop = document.getElementById('stop')
const pill = document.getElementById('pill')
const bell = document.getElementById('bell')
const who = document.getElementById('who')

const page = {
  // id is the number this browser was given, which says where what it sends
  // goes. A stream that ended is a number that is no longer answered on.
  id: null,
  // oldest is the oldest stored message on the screen, which is what asking
  // for more looks back from.
  oldest: null,
  // reading says the screen is at the bottom, where what arrives is shown
  // rather than kept behind a pill.
  reading: true,
  // mine says the reply being written answers something sent from here, and
  // told that it has been shown once already.
  mine: false,
  told: false,
  // waiting are the pictures picked but not sent yet.
  waiting: [],
  asking: false,
}

// A message a browser that was away missed is read back from the conversation,
// so the only thing kept here is what is on the screen.

function stream() {
  const es = new EventSource('api/events')
  es.addEventListener('open', () => document.body.classList.remove('away'))
  es.addEventListener('error', () => document.body.classList.add('away'))
  es.addEventListener('sync', e => synced(JSON.parse(e.data)))
  es.addEventListener('said', e => shows(...stored(JSON.parse(e.data))))
  es.addEventListener('message', e => {
    const m = JSON.parse(e.data)
    note.hidden = true
    shows(bubbled(m))
    told(m)
  })
  es.addEventListener('edit', e => {
    const m = JSON.parse(e.data)
    const el = chat.querySelector(`[data-key="b${m.id}"]`)
    if (el) {
      note.hidden = true
      words(el, m.text)
      if (page.reading) bottom()
    }
  })
  // A note is what she is doing in the middle of a reply, shown under the dots
  // until the reply goes on.
  es.addEventListener('note', e => {
    note.textContent = JSON.parse(e.data).text
    note.hidden = false
  })
  es.addEventListener('replying', e => {
    const on = JSON.parse(e.data).on
    writing.hidden = !on
    stop.hidden = !on
    if (!on) {
      page.mine = false
      note.hidden = true
    }
  })
  es.addEventListener('commands', e => offers(JSON.parse(e.data).commands))
}

// synced opens the page on the conversation. A browser that missed nothing
// keeps what it is reading, and is given only the number it is answered on.
function synced(s) {
  page.id = s.page
  if (s.character) {
    who.textContent = s.character
    document.title = s.character
  }
  if (s.caught) return
  chat.replaceChildren()
  page.oldest = null
  for (const m of s.messages || []) chat.append(...stored(m))
  dates()
  bottom()
}

// stored is a message of the conversation, as the bubbles it reads as. A reply
// she wrote as several texts is read back as the texts she wrote, since that is
// how it arrived; what was sent to her is one thing that was sent.
function stored(m) {
  if (!page.oldest || m.id < page.oldest) page.oldest = m.id
  const hers = m.role === 'assistant'
  const texts = hers ? paragraphs(m.text) : [m.text]
  const out = []
  texts.forEach((text, i) => {
    const el = bubble(hers ? 'hers' : 'mine', 'm' + m.id + '-' + i, m.at)
    // A message of another frontend is the same person speaking, elsewhere.
    if (i === 0 && !hers && m.channel && m.channel !== 'web') {
      const from = document.createElement('span')
      from.className = 'from'
      from.textContent = 'from ' + m.channel
      el.append(from)
    }
    words(el, text, true)
    out.push(el)
  })
  const last = out.at(-1) || bubble(hers ? 'hers' : 'mine', 'm' + m.id + '-0', m.at)
  if (out.length === 0) out.push(last)
  for (const sha of m.pictures || []) {
    const img = document.createElement('img')
    img.src = 'api/media/' + sha
    img.alt = 'a picture'
    img.loading = 'lazy'
    last.append(img)
  }
  // A bubble holding a picture and nothing else is the picture.
  for (const el of out) {
    if (el.textContent === '' && el.children.length > 0) el.classList.add('picture')
  }
  return out
}

// paragraphs are the texts a message reads as: a blank line is where one ends
// and the next begins, which is how she writes them.
function paragraphs(text) {
  return (text || '').split(/\n[ \t]*\n/).map(t => t.trim()).filter(t => t !== '')
}

// bubbled is one thing she said, or one the session said itself.
function bubbled(m) {
  const el = bubble(m.hers ? 'hers' : 'aside', 'b' + m.id, null)
  words(el, m.text)
  if (m.choices) el.append(offered(m.choices))
  return el
}

function bubble(kind, key, at) {
  const el = document.createElement('div')
  el.className = 'bubble ' + kind
  el.dataset.key = key
  el.dataset.at = at || new Date().toISOString()
  return el
}

// offered is what can be picked from a message. What comes back is the
// session's own word for the choice, handed back as it was given.
function offered(choices) {
  const row = document.createElement('div')
  row.className = 'choices'
  for (const c of choices) {
    const b = document.createElement('button')
    b.type = 'button'
    b.textContent = c.label
    if (c.current) b.className = 'current'
    b.addEventListener('click', () => sends('api/pick', JSON.stringify({ picked: c.picked }), 'application/json'))
    row.append(b)
  }
  return row
}

// words writes a text into a bubble as nodes. Nothing the conversation says is
// ever read as markup: what is bold, in code or a link is built here.
const marks = /`([^`]+)`|\*\*([^*]+)\*\*|\*([^*\n]+)\*|(https?:\/\/[^\s<>"]+)/g

function words(el, text, keep) {
  const from = keep ? el.querySelector('.from') : null
  el.replaceChildren()
  if (from) el.append(from)
  let at = 0
  for (const m of text.matchAll(marks)) {
    if (m.index > at) el.append(text.slice(at, m.index))
    if (m[1] !== undefined) el.append(tag('code', m[1]))
    else if (m[2] !== undefined) el.append(tag('strong', m[2]))
    else if (m[3] !== undefined) el.append(tag('em', m[3]))
    else {
      const a = document.createElement('a')
      a.href = m[4]
      a.textContent = m[4]
      a.target = '_blank'
      a.rel = 'noreferrer noopener'
      el.append(a)
    }
    at = m.index + m[0].length
  }
  if (at < text.length) el.append(text.slice(at))
}

function tag(name, text) {
  const el = document.createElement(name)
  el.textContent = text
  return el
}

// shows puts something at the end of the conversation, and keeps the screen
// where it is unless it is at the bottom: what is being read stays read.
function shows(...els) {
  chat.append(...els)
  dates()
  if (page.reading) bottom()
  else pill.hidden = false
}

function bottom() {
  chat.scrollTop = chat.scrollHeight
  pill.hidden = true
}

// dates puts a day between the messages of different days, and takes away the
// ones that no longer separate anything.
function dates() {
  let day = null
  for (const el of Array.from(chat.children)) {
    if (el.className === 'day') {
      el.remove()
      continue
    }
    const on = new Date(el.dataset.at)
    const name = named(on)
    if (name !== day) {
      day = name
      const sep = document.createElement('div')
      sep.className = 'day'
      sep.textContent = name
      el.before(sep)
    }
  }
}

function named(on) {
  const today = new Date()
  const day = 24 * 60 * 60 * 1000
  const same = (a, b) => a.toDateString() === b.toDateString()
  if (same(on, today)) return 'Today'
  if (same(on, new Date(today.getTime() - day))) return 'Yesterday'
  return on.toLocaleDateString(undefined, { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric' })
}

// earlier asks for what came before what is on the screen, and keeps the
// screen where it is while it grows upwards.
async function earlier() {
  if (page.asking || !page.oldest || !page.id) return
  page.asking = true
  try {
    const resp = await fetch('api/history?before=' + page.oldest, { headers: { 'X-Paula-Page': page.id } })
    if (!resp.ok) return
    const older = (await resp.json()).messages || []
    if (older.length === 0) return
    const was = chat.scrollHeight
    const first = chat.firstChild
    for (const m of older) {
      for (const el of stored(m)) chat.insertBefore(el, first)
    }
    dates()
    chat.scrollTop += chat.scrollHeight - was
  } finally {
    page.asking = false
  }
}

chat.addEventListener('scroll', () => {
  page.reading = chat.scrollHeight - chat.scrollTop - chat.clientHeight < 40
  if (page.reading) pill.hidden = true
  if (chat.scrollTop < 60) earlier()
})

pill.addEventListener('click', () => {
  page.reading = true
  bottom()
})

// sends something to this page's session. A stream that ended leaves the
// number it was answered on behind, so what was typed goes once the browser
// has opened another.
async function sends(path, body, kind) {
  for (let again = 0; again < 2; again++) {
    if (!page.id) await opened()
    const headers = { 'X-Paula-Page': page.id }
    if (kind) headers['Content-Type'] = kind
    let resp
    try {
      resp = await fetch(path, { method: 'POST', headers, body })
    } catch {
      resp = null
    }
    if (resp && resp.ok) return true
    if (resp && resp.status !== 410) {
      aside(await resp.text())
      return false
    }
    page.id = null
  }
  aside('that did not go through')
  return false
}

// opened waits for the browser to be given a page to be answered on.
function opened() {
  return new Promise(done => {
    const at = setInterval(() => {
      if (!page.id) return
      clearInterval(at)
      done()
    }, 200)
    setTimeout(() => {
      clearInterval(at)
      done()
    }, 5000)
  })
}

// aside says something of the page's own, which is nobody talking.
function aside(text) {
  const el = bubble('aside', 'p' + Date.now(), null)
  words(el, text)
  shows(el)
}

// offers the commands that are worth a button of their own. What can be typed
// is what the session says it is.
const handy = ['/models', '/summary', '/memory', '/help']

function offers(commands) {
  quick.replaceChildren()
  for (const c of commands) {
    const name = '/' + c.name
    if (!handy.includes(name)) continue
    const b = document.createElement('button')
    b.type = 'button'
    b.textContent = name
    b.title = c.short
    b.addEventListener('click', () => says(name, []))
    quick.append(b)
  }
}

// says posts a message, and puts it on the screen at once: a session is not
// shown what was typed into it.
async function says(text, images) {
  const body = new FormData()
  body.append('text', text)
  for (const p of images) body.append('image', p.file)

  const el = bubble('mine', 'p' + Date.now(), null)
  words(el, text)
  for (const p of images) {
    const img = document.createElement('img')
    img.src = p.url
    img.alt = 'a picture'
    el.append(img)
  }
  page.reading = true
  shows(el)

  page.mine = true
  page.told = false
  await sends('api/messages', body, null)
}

composer.addEventListener('submit', e => {
  e.preventDefault()
  const text = typed.value.trim()
  if (!text && page.waiting.length === 0) return
  const images = page.waiting
  page.waiting = []
  previews()
  typed.value = ''
  grows()
  says(text, images)
})

// Enter sends and shift with it starts a line, the way a chat does.
typed.addEventListener('keydown', e => {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault()
    composer.requestSubmit()
  }
})

typed.addEventListener('input', grows)

// grows makes the box as tall as what is typed in it, up to what the page
// gives it. What it holds is measured without the border around it, and the
// box is measured with it, so the border is added back: a box that is short of
// its own content by that much scrolls while it looks empty.
function grows() {
  typed.style.height = 'auto'
  const style = getComputedStyle(typed)
  const border = parseFloat(style.borderTopWidth) + parseFloat(style.borderBottomWidth)
  const most = parseFloat(style.maxHeight)
  const tall = typed.scrollHeight + border
  typed.style.height = Math.min(tall, most) + 'px'
  typed.style.overflowY = tall > most ? 'auto' : 'hidden'
}

stop.addEventListener('click', () => sends('api/stop', null, null))

document.getElementById('clip').addEventListener('click', () => picked.click())

picked.addEventListener('change', () => {
  for (const file of picked.files) holds(file)
  picked.value = ''
})

// A picture pasted into the page is one to send, the way one picked is.
typed.addEventListener('paste', e => {
  for (const item of e.clipboardData.files) {
    if (item.type.startsWith('image/')) holds(item)
  }
})

function holds(file) {
  page.waiting.push({ file, url: URL.createObjectURL(file) })
  previews()
}

function previews() {
  pictures.replaceChildren()
  page.waiting.forEach((p, i) => {
    const fig = document.createElement('figure')
    const img = document.createElement('img')
    img.src = p.url
    img.alt = p.file.name || 'a picture'
    const drop = document.createElement('button')
    drop.type = 'button'
    drop.textContent = '×'
    drop.title = 'take it out'
    drop.addEventListener('click', () => {
      URL.revokeObjectURL(p.url)
      page.waiting.splice(i, 1)
      previews()
    })
    fig.append(img, drop)
    pictures.append(fig)
  })
}

// told shows the first of a reply to something sent from here while the page
// is not being looked at. A conversation nobody is watching is one worth being
// told about; the rest of the reply is read when the page is opened.
function told(m) {
  if (!m.hers || !page.mine || page.told || !document.hidden) return
  page.told = true
  if (Notification.permission !== 'granted') return
  navigator.serviceWorker.ready.then(reg => {
    reg.showNotification(who.textContent, { body: m.text, tag: 'paula' })
  }).catch(() => {})
}

// The bell asks for the permission, which a browser only asks for on something
// the person did. It is offered where notifications can work at all.
if ('Notification' in window && 'serviceWorker' in navigator && window.isSecureContext) {
  if (Notification.permission === 'granted') navigator.serviceWorker.register('sw.js')
  else if (Notification.permission !== 'denied') bell.hidden = false
}

bell.addEventListener('click', async () => {
  bell.hidden = true
  if (await Notification.requestPermission() === 'granted') {
    navigator.serviceWorker.register('sw.js')
  }
})

grows()
stream()
