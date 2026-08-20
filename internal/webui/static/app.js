'use strict';

// The browser talks to the player's own BRC-100 wallet — BSV Desktop, or anything else the SDK's
// substrate can reach. The key never comes here: the page asks the wallet, and the wallet asks the
// player. Signing happens inside the wallet, which is the whole point of using BRC-100 rather than
// asking a player to hand a key to a web page.

const el = (id) => document.getElementById(id);
const RED = new Set(['h', 'd']);
const SUIT = { s: '♠', h: '♥', d: '♦', c: '♣' };

// wallet is the connected BRC-100 wallet. identityKey is its public identity, which is how the
// table knows which seat is acting.
let wallet = null;
let identityKey = '';
let mySeat = -1;

async function getJSON(url) {
  const res = await fetch(url, { headers: { accept: 'application/json' } });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || `${url} -> ${res.status}`);
  return body;
}

async function postJSON(url, payload) {
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(payload),
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || `${url} -> ${res.status}`);
  return body;
}

// --- the wallet -----------------------------------------------------------
//
// The player runs a BRC-100 wallet on their own machine and the page talks to it directly. This is
// not a fallback for a missing browser wallet: the wallet process holds the per-hand masking
// secrets that keep hole cards private, and no published BRC-100 wallet exposes the operation
// needed to strip a mask. See docs/wallet-native-deal.md.

function card(spec) {
  const d = document.createElement('div');
  if (!spec) { d.className = 'card empty'; return d; }
  const suit = spec.slice(-1), rank = spec.slice(0, -1);
  d.className = RED.has(suit) ? 'card red' : 'card';
  d.textContent = `${rank}${SUIT[suit] || suit}`;
  return d;
}
function back() {
  const d = document.createElement('div');
  d.className = 'card back'; d.textContent = '??';
  return d;
}
function badge(text, kind) {
  const s = document.createElement('span');
  s.className = kind ? `badge ${kind}` : 'badge';
  s.textContent = text; return s;
}
function setStatus(id, msg, kind) {
  const n = el(id);
  n.textContent = msg;
  n.className = kind ? `status ${kind}` : 'status';
}

async function loadInfo() {
  try {
    const info = await getJSON('/api/info');
    el('network').textContent = info.network || '?';
    el('version').textContent = info.version || '';
    // Fill the commands with this page's own origin, so they can be pasted without editing.
    for (const id of ['originHost', 'originEcho', 'originEcho2']) {
      const n = el(id);
      if (n) n.textContent = window.location.origin;
    }
  } catch (e) {
    setStatus('connectStatus', `Could not reach the table service: ${e.message}`, 'bad');
  }
}

// agentBase is the wallet the player is running, as typed into the page.
let agentBase = '';

function agentURL() {
  return el('agentUrl').value.trim().replace(/\/+$/, '');
}

async function showBalance() {
  try {
    const r = await fetch(`${agentBase}/identity`);
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    const info = await r.json();
    if (typeof info.balanceSatoshis === 'number') {
      el('balance').textContent = `balance ${info.balanceSatoshis.toLocaleString()} sat`;
      return info.balanceSatoshis;
    }
    el('balance').textContent = 'balance unknown';
  } catch (e) {
    el('balance').textContent = 'balance unavailable';
  }
  return null;
}

el('connect').addEventListener('click', async () => {
  agentBase = agentURL();
  if (!agentBase) {
    setStatus('connectStatus', 'Enter the address your wallet is listening on.', 'bad');
    return;
  }
  setStatus('connectStatus', `Asking ${agentBase} who it speaks for…`);
  try {
    const r = await fetch(`${agentBase}/identity`);
    if (!r.ok) throw new Error(`the wallet answered HTTP ${r.status}`);
    const info = await r.json();
    if (!info.identityKey) throw new Error('the wallet did not return an identity key');
    identityKey = info.identityKey;

    setStatus('connectStatus',
      `Connected to your wallet. Your identity is ${identityKey.slice(0, 20)}…`, 'ok');

    el('fundPanel').hidden = false;
    el('seatPanel').hidden = false;
    await showBalance();
    await refresh();
  } catch (e) {
    setStatus('connectStatus',
      `Your wallet did not answer: ${e.message}. Check the agent is still running, that you ` +
      `started it with -origin ${window.location.origin}, and that the address is reachable ` +
      `from this browser.`, 'bad');
  }
});

el('refreshBalance').addEventListener('click', showBalance);

el('faucet').addEventListener('click', async () => {
  setStatus('fundStatus', 'Claiming from the teratestnet faucet…');
  try {
    const r = await fetch(`${agentBase}/faucet`, { method: 'POST' });
    const body = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(body.error || `HTTP ${r.status}`);
    setStatus('fundStatus',
      `Claimed ${Number(body.satoshis || 0).toLocaleString()} sat. ` +
      `The wallet recorded the derivation material, so it is spendable.`, 'ok');
    await showBalance();
  } catch (e) {
    setStatus('fundStatus', `The faucet claim failed: ${e.message}`, 'bad');
  }
});

el('join').addEventListener('click', async () => {
  try {
    // The wallet is always registered: a dealerless deal is the only kind this page offers, and
    // the table cannot sequence one without knowing where the seat's wallet is.
    const { seat, agentRegistered } = await postJSON('/api/join', {
      identityKey, agentUrl: agentBase,
    });
    mySeat = seat;
    if (!agentRegistered) {
      setStatus('seatStatus',
        `Seated at ${seat}, but the table could not register your wallet, so this hand would ` +
        `not be dealerless. Check the wallet address and rejoin.`, 'bad');
      return;
    }
    setStatus('seatStatus', `You are seat ${seat}. Commit your buy-in when ready.`, 'ok');
    await refresh();
  } catch (e) {
    setStatus('seatStatus', e.message, 'bad');
  }
});

el('ready').addEventListener('click', async () => {
  try {
    await postJSON('/api/ready', { identityKey });
    setStatus('seatStatus', 'Buy-in committed. The hand starts when every seat is ready.', 'ok');
    await refresh();
  } catch (e) {
    setStatus('seatStatus', e.message, 'bad');
  }
});

for (const b of document.querySelectorAll('button.act')) {
  b.addEventListener('click', async () => {
    const action = b.dataset.act;
    const to = action === 'raise' ? Number(el('raiseTo').value || 0) : 0;
    el('actError').hidden = true;
    try {
      await postJSON('/api/act', { identityKey, action, to });
      await refresh();
    } catch (e) {
      // The engine refused. Show exactly why: it is the authoritative answer and it explains
      // itself better than a generic message would.
      el('actError').textContent = e.message;
      el('actError').hidden = false;
    }
  });
}

async function refresh() {
  if (!identityKey) return;
  let data;
  try {
    data = await getJSON(`/api/live?identityKey=${encodeURIComponent(identityKey)}`);
  } catch (e) {
    return;
  }
  mySeat = data.seat;
  const v = data.table, legal = data.legal || {};

  el('tablePanel').hidden = false;
  el('tableId').textContent = v.tableId;
  el('phase').textContent = v.phase;
  el('street').textContent = v.street || '';
  el('street').hidden = !v.street;
  el('youAre').textContent = mySeat >= 0 ? `you are seat ${mySeat}` : 'observing';

  // Say which kind of deal this was. A player told nothing would reasonably assume the
  // stronger one, so the weaker one is labelled explicitly.
  const dealTag = el('dealKind');
  if (v.street) {
    dealTag.hidden = false;
    if (data.dealerless) {
      dealTag.textContent = 'dealerless';
      dealTag.className = 'pill';
      dealTag.title = 'Each seat held its own cards. Nothing else, including this server, could read them.';
    } else {
      dealTag.textContent = 'server-dealt';
      dealTag.className = 'pill warn';
      dealTag.title = 'A seat had no agent, so the server shuffled. It can see the cards.';
    }
  } else dealTag.hidden = true;

  el('stakes').innerHTML = '';
  const stake = (l, val) => {
    const d = document.createElement('div');
    d.innerHTML = `${l} <b>${val}</b>`;
    el('stakes').appendChild(d);
  };
  stake('Seats', v.seats);
  stake('Buy-in', `${v.buyInSatoshis} sat`);
  stake('Blinds', `${v.smallBlind}/${v.bigBlind}`);
  if (v.refundLockHeight) stake('Refund at block', v.refundLockHeight);

  const board = el('board');
  board.innerHTML = '';
  for (let i = 0; i < 5; i++) board.appendChild(card((v.board || [])[i]));
  el('pot').textContent = `Pot ${v.pot ?? 0} sat`;

  const stall = el('stall');
  if (v.stallReason) {
    const who = v.stalledSeat >= 0 ? `seat ${v.stalledSeat}` : 'a seat';
    stall.textContent = `Hand stalled by ${who}: ${v.stallReason}. Refunds recoverable from block ${v.refundLockHeight}.`;
    stall.hidden = false;
  } else stall.hidden = true;

  // The result, once the hand is over.
  const result = el('result');
  const winners = data.winners || {};
  const winnerSeats = Object.keys(winners);
  if (winnerSeats.length) {
    const mine = winners[String(mySeat)];
    const parts = winnerSeats.map((s) => `seat ${s} +${winners[s]}`);
    result.textContent = mine
      ? `Hand complete. You won ${mine} sat. (${parts.join(', ')})`
      : `Hand complete. ${parts.join(', ')}.`;
    result.hidden = false;
  } else result.hidden = true;

  // Offer exactly the actions the engine says are legal. Offering one it will refuse is worse
  // than offering none: the player clicks, is told no, and learns nothing.
  const bar = el('actions');
  if (legal.yourTurn) {
    bar.hidden = false;
    const show = (sel, on) => {
      const b = bar.querySelector(sel);
      b.hidden = !on; b.disabled = !on;
    };
    show('[data-act=fold]', legal.canFold);
    show('[data-act=check]', legal.canCheck);
    show('[data-act=call]', legal.canCall);
    bar.querySelector('[data-act=call]').textContent =
      legal.canCall ? `Call ${legal.callAmount}` : 'Call';
    const rg = bar.querySelector('.raiseGroup');
    rg.hidden = !legal.canBetRaise;
    if (legal.canBetRaise) {
      const input = el('raiseTo');
      input.min = legal.minTo; input.max = legal.maxTo;
      if (!input.value || Number(input.value) < legal.minTo) input.value = legal.minTo;
      bar.querySelector('[data-act=raise]').textContent = `Raise to (${legal.minTo}–${legal.maxTo})`;
    }
  } else bar.hidden = true;

  const seats = el('seats');
  seats.innerHTML = '';
  for (const p of v.players || []) {
    const row = document.createElement('div');
    row.className = 'seat';
    if (p.seat === v.toAct) row.classList.add('toact');
    if (p.seat === mySeat) row.classList.add('you');
    if (p.folded) row.classList.add('folded');

    const top = document.createElement('div');
    top.className = 'seatTop';
    const name = document.createElement('span');
    name.className = 'seatName';
    name.textContent = p.seat === mySeat
      ? `Seat ${p.seat} · you`
      : `Seat ${p.seat} · ${p.identityKey || 'empty'}`;
    top.appendChild(name);
    if (p.seat === v.toAct) top.appendChild(badge('to act', 'ok'));
    if (p.folded) top.appendChild(badge('folded'));
    if (p.allIn) top.appendChild(badge('all in', 'warn'));
    if (p.refundHeld) top.appendChild(badge('refund held', 'ok'));
    if (p.atRisk) top.appendChild(badge('at risk', 'warn'));
    const stacks = document.createElement('span');
    stacks.className = 'stacks';
    stacks.textContent = `${p.stack} sat` + (p.committed ? ` · in ${p.committed}` : '');
    top.appendChild(stacks);
    row.appendChild(top);

    // Only this seat's own cards are ever present in the response, so there is nothing here
    // for a browser to leak.
    const cr = document.createElement('div');
    cr.className = 'seatCards';
    if (p.hole && p.hole.length) for (const c of p.hole) cr.appendChild(card(c));
    else { cr.appendChild(back()); cr.appendChild(back()); }
    row.appendChild(cr);

    if (p.moneySummary) {
      const m = document.createElement('div');
      m.className = 'seatMoney'; m.textContent = p.moneySummary;
      row.appendChild(m);
    }
    seats.appendChild(row);
  }
  el('updated').textContent = `Updated ${new Date(v.updatedAt).toLocaleTimeString()}`;
}

loadInfo();
// Poll rather than stream: a dropped socket that silently stops updating is worse than a poll that
// visibly fails, and this view is small.
setInterval(refresh, 2000);
