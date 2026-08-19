'use strict';

// The UI is a renderer. It holds no keys, signs nothing, and cannot mutate game state — a browser
// tab is not a place to keep a private key, and the agent already exists to hold one.

const el = (id) => document.getElementById(id);
const RED = new Set(['h', 'd']);

async function getJSON(url) {
  const res = await fetch(url, { headers: { accept: 'application/json' } });
  if (!res.ok) throw new Error(`${url} -> ${res.status}`);
  return res.json();
}

// A card string is like "Ah" or "Td": rank then suit letter.
function cardEl(spec) {
  const d = document.createElement('div');
  if (!spec) {
    d.className = 'card empty';
    return d;
  }
  const suit = spec.slice(-1);
  const rank = spec.slice(0, -1);
  d.className = RED.has(suit) ? 'card red' : 'card';
  const glyph = { s: '♠', h: '♥', d: '♦', c: '♣' }[suit] || suit;
  d.textContent = `${rank}${glyph}`;
  return d;
}

function faceDownEl() {
  const d = document.createElement('div');
  d.className = 'card back';
  d.textContent = '??';
  return d;
}

function badge(text, kind) {
  const s = document.createElement('span');
  s.className = kind ? `badge ${kind}` : 'badge';
  s.textContent = text;
  return s;
}

async function loadInfo() {
  try {
    const info = await getJSON('/api/info');
    el('network').textContent = info.network || 'unknown';
    el('version').textContent = info.version || '';
    el('custody').textContent = info.custody || '';
    if (info.identityKey) el('tableKey').textContent = info.identityKey;
  } catch (e) {
    el('custody').textContent = `Could not reach the service: ${e.message}`;
  }
}

async function loadTables() {
  const sel = el('tableSelect');
  const previous = sel.value;
  let tables = [];
  try {
    ({ tables } = await getJSON('/api/tables'));
  } catch (e) {
    // A failed list is not the same as no tables; say so rather than showing an empty lobby.
    el('emptyPanel').querySelector('.hint').textContent =
      `Could not list tables: ${e.message}`;
    return;
  }

  sel.innerHTML = '';
  if (!tables.length) {
    sel.appendChild(new Option('— none —', ''));
    el('tablePanel').hidden = true;
    el('emptyPanel').hidden = false;
    return;
  }
  for (const t of tables) {
    sel.appendChild(new Option(`${t.tableId} (${t.phase})`, t.tableId));
  }
  // Keep the operator's selection across refreshes.
  sel.value = tables.some((t) => t.tableId === previous) ? previous : tables[0].tableId;
  el('emptyPanel').hidden = true;
  await loadTable();
}

async function loadTable() {
  const id = el('tableSelect').value;
  if (!id) return;
  const seat = el('seatSelect').value;

  let v;
  try {
    v = await getJSON(`/api/table?id=${encodeURIComponent(id)}&seat=${encodeURIComponent(seat)}`);
  } catch (e) {
    el('tablePanel').hidden = true;
    return;
  }

  el('tablePanel').hidden = false;
  el('tableId').textContent = v.tableId;
  el('phase').textContent = v.phase || '—';
  el('street').textContent = v.street || '';
  el('street').hidden = !v.street;

  el('stakes').innerHTML = '';
  const stake = (label, value) => {
    const d = document.createElement('div');
    d.innerHTML = `${label} <b>${value}</b>`;
    el('stakes').appendChild(d);
  };
  stake('Seats', v.seats);
  stake('Buy-in', `${v.buyInSatoshis} sat`);
  stake('Blinds', `${v.smallBlind}/${v.bigBlind}`);
  if (v.refundLockHeight) stake('Refund at block', v.refundLockHeight);

  // The board. Five slots always, so the shape of the hand is visible before the river.
  const board = el('board');
  board.innerHTML = '';
  for (let i = 0; i < 5; i++) {
    board.appendChild(cardEl((v.board || [])[i]));
  }
  el('pot').textContent = `Pot ${v.pot ?? 0} sat`;

  // A stall is surfaced, never hidden: a player whose money is stuck is entitled to know.
  const stall = el('stall');
  if (v.stallReason) {
    const who = v.stalledSeat >= 0 ? `seat ${v.stalledSeat}` : 'a seat';
    stall.textContent = `Hand stalled by ${who}: ${v.stallReason}. Refunds are recoverable from block ${v.refundLockHeight}.`;
    stall.hidden = false;
  } else {
    stall.hidden = true;
  }

  const seats = el('seats');
  seats.innerHTML = '';
  for (const p of v.players || []) {
    const row = document.createElement('div');
    row.className = 'seat';
    if (p.seat === v.toAct) row.classList.add('toact');
    if (p.folded) row.classList.add('folded');

    const top = document.createElement('div');
    top.className = 'seatTop';
    const name = document.createElement('span');
    name.className = 'seatName';
    name.textContent = `Seat ${p.seat} · ${p.identityKey || 'empty'}`;
    top.appendChild(name);

    if (p.seat === v.toAct) top.appendChild(badge('to act', 'ok'));
    if (p.folded) top.appendChild(badge('folded'));
    if (p.allIn) top.appendChild(badge('all in', 'warn'));
    if (p.refundHeld) top.appendChild(badge('refund held', 'ok'));
    if (p.funded) top.appendChild(badge('funded', 'ok'));
    if (p.atRisk) top.appendChild(badge('at risk', 'warn'));

    const stacks = document.createElement('span');
    stacks.className = 'stacks';
    stacks.textContent = `${p.stack} sat` + (p.committed ? ` · in ${p.committed}` : '');
    top.appendChild(stacks);
    row.appendChild(top);

    // Hole cards: only ever present for the requesting seat. Everyone else shows backs,
    // because the server does not send another seat's cards at all.
    const cardRow = document.createElement('div');
    cardRow.className = 'seatCards';
    if (p.hole && p.hole.length) {
      for (const c of p.hole) cardRow.appendChild(cardEl(c));
    } else {
      cardRow.appendChild(faceDownEl());
      cardRow.appendChild(faceDownEl());
    }
    row.appendChild(cardRow);

    if (p.moneySummary) {
      const m = document.createElement('div');
      m.className = 'seatMoney';
      m.textContent = p.moneySummary;
      row.appendChild(m);
    }
    seats.appendChild(row);
  }

  el('updated').textContent = `Updated ${new Date(v.updatedAt).toLocaleTimeString()}`;
}

el('refresh').addEventListener('click', () => { loadTables(); });
el('tableSelect').addEventListener('change', () => { loadTable(); });
el('seatSelect').addEventListener('change', () => { loadTable(); });

loadInfo();
loadTables();
// Poll rather than stream: the view is small, and a dropped WebSocket that silently stops
// updating is worse than a poll that visibly fails.
setInterval(loadTables, 4000);
