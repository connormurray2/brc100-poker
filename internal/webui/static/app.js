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
    // The page knows the table's identity key and its own origin, so the command it shows is
    // complete: a player copies it and runs it, with nothing left to look up or substitute.
    // Serving one table is what makes this possible.
    const origin = window.location.origin;
    const n = el('originEcho2');
    if (n) n.textContent = origin;
    if (info.identityKey) {
      agentCommand = [
        'go run ./cmd/agent \\',
        '  -key    secrets/player.key \\',
        '  -db     secrets/player.db \\',
        `  -table  ${info.identityKey} \\`,
        `  -origin ${origin} \\`,
        '  -listen 127.0.0.1:8091',
      ].join('\n');
      el('agentCommand').textContent = agentCommand;
    } else {
      el('agentCommand').textContent =
        'The table did not report its identity key, so this command cannot be completed.';
    }
  } catch (e) {
    setStatus('connectStatus', `Could not reach the table service: ${e.message}`, 'bad');
  }
}

// agentBase is the wallet the player is running, as typed into the page.
let agentBase = '';

// agentCommand is the fully-populated command shown to the player, kept so the copy button and
// the visible text can never disagree.
let agentCommand = '';

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
    // Start relaying immediately: the table may ask this wallet to deal at any point after the
    // seat is taken, and a relay that only starts on join would miss the first request.
    startRelay();
    await showBalance();
    await refresh();
  } catch (e) {
    setStatus('connectStatus',
      `Your wallet did not answer: ${e.message}. Check the agent is still running, that you ` +
      `started it with -origin ${window.location.origin}, and that the address is reachable ` +
      `from this browser.`, 'bad');
  }
});


// --- the relay ------------------------------------------------------------
//
// The table runs elsewhere and cannot dial a wallet on this machine: 127.0.0.1 there means the
// server, which is what made a remote hand fail with "connection refused". This page can reach
// both, so it carries the traffic.
//
// It is a pipe and nothing more. Each request is already signed by the table and addressed to
// this seat's identity key, and each response is signed by the wallet, so neither side has to
// trust the page. Tampering, replaying or fabricating a message fails verification at the ends.
let relayTimer = null;

async function relayOnce() {
  if (!identityKey || !agentBase) return;
  let items = [];
  try {
    const r = await postJSON('/api/relay/poll', { identityKey });
    items = r.requests || [];
  } catch {
    return; // the table is unreachable for a moment; the next tick retries
  }

  for (const item of items) {
    try {
      const res = await fetch(agentBase, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(item.body),
      });
      const text = await res.text();
      let body;
      try {
        body = JSON.parse(text);
      } catch {
        throw new Error(`the wallet returned ${res.status} and not JSON`);
      }
      await postJSON('/api/relay/reply', { identityKey, nonce: item.nonce, body });
    } catch (e) {
      // Report the failure rather than letting the table wait out its timeout: the player
      // then learns their wallet stopped answering, instead of the hand simply stalling.
      await postJSON('/api/relay/reply', {
        identityKey, nonce: item.nonce, error: String(e.message || e),
      }).catch(() => {});
    }
  }
}

function startRelay() {
  if (relayTimer) return;
  // Arming rides the same timer: a pot appears between hands, and the wallet must know about it
  // before the table asks for a settlement signature.
  setInterval(armWallet, 1200);
  // 400ms is short enough that a deal -- a few dozen round trips -- completes promptly, and long
  // enough that an idle table is not being hammered.
  relayTimer = setInterval(relayOnce, 400);
}


// --- arming the wallet ----------------------------------------------------
//
// Before a settlement can be signed, this seat's own wallet must record what it expects: which pot
// its stake is in, what it should be paid, and what fee it will tolerate. The table cannot do this
// -- a stake the table wrote would encode the table's expectation, which would make the wallet's
// signing check a rubber stamp. So the page asks the table for the amounts and derivation
// material, and the wallet derives the payout scripts itself.
const armedHands = new Set();

async function armWallet() {
  if (!identityKey || !agentBase) return;
  let info;
  try {
    info = await postJSON('/api/stake', { identityKey });
  } catch {
    return;
  }
  if (!info.open || !info.stake || !info.refundTxHex) return;
  const stake = info.stake;
  // Only once per hand: the wallet would accept a re-record, but there is nothing to gain and
  // a repeated call on every poll is noise.
  if (armedHands.has(stake.handId)) return;
  if (!stake.payouts || !stake.payouts.length) return;

  const req = {
    handId: stake.handId,
    potTxid: stake.potTxid,
    potVout: stake.potVout,
    potSatoshis: stake.potSatoshis,
    potScriptHex: stake.potScriptHex,
    seat: info.seat,
    senderIdentityKey: stake.senderIdentityKey,
    payouts: stake.payouts,
    maxFee: stake.maxFee,
    refundTxHex: info.refundTxHex,
  };
  try {
    await callWallet('recordStake', req);
    armedHands.add(stake.handId);
    setStatus('seatStatus',
      `Your wallet is holding a signed refund for this pot and knows what it expects to be ` +
      `paid. It will refuse any other settlement.`, 'ok');
  } catch (e) {
    setStatus('seatStatus', `Your wallet would not record this stake: ${e.message}`, 'bad');
  }
}

// callWallet makes an authenticated substrate call from this page to the local wallet.
//
// The page cannot sign as the wallet's owner, so this relies on the wallet trusting requests from
// an allowed origin for owner-only methods. That is the same trust boundary as the funding button.
async function callWallet(method, params) {
  const res = await fetch(`${agentBase}/owner/${method}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(params),
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || `HTTP ${res.status}`);
  return body;
}

el('copyCommand').addEventListener('click', async () => {
  if (!agentCommand) return;
  try {
    await navigator.clipboard.writeText(agentCommand);
  } catch {
    // Clipboard access is refused on an insecure origin and by some browsers. Selecting the
    // text is a worse experience than copying it, but it beats a button that does nothing.
    const range = document.createRange();
    range.selectNodeContents(el('agentCommand'));
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  }
  const state = el('copyState');
  state.hidden = false;
  setTimeout(() => { state.hidden = true; }, 1500);
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
    const { seat, agentRegistered, relayed } = await postJSON('/api/join', {
      identityKey, relay: true,
    });
    mySeat = seat;
    if (!agentRegistered) {
      setStatus('seatStatus',
        `Seated at ${seat}, but the table could not register your wallet, so this hand would ` +
        `not be dealerless. Check the wallet address and rejoin.`, 'bad');
      return;
    }
    const how = relayed
      ? 'This page will carry deal traffic to your wallet, so keep the tab open.'
      : 'Your wallet is registered directly.';
    setStatus('seatStatus', `You are seat ${seat}. ${how} Commit your buy-in when ready.`, 'ok');
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

  // Say whose turn it is in words, above the felt. A badge on a seat row was too easy to miss,
  // and a player who cannot tell whether they are holding up the table will sit and wait.
  const banner = el('turnBanner');
  if (v.phase === 'hand complete') {
    banner.hidden = false;
    banner.className = 'turnBanner';
    banner.textContent = 'Hand complete. The next hand starts in a few seconds.';
  } else if (legal.yourTurn) {
    banner.hidden = false;
    banner.className = 'turnBanner mine';
    banner.textContent = 'Your turn — everyone is waiting on you.';
  } else if (typeof v.toAct === 'number' && v.toAct >= 0) {
    banner.hidden = false;
    banner.className = 'turnBanner waiting';
    banner.textContent = `Waiting for seat ${v.toAct}${v.toAct === mySeat ? ' (you)' : ''}…`;
  } else if (v.phase) {
    banner.hidden = false;
    banner.className = 'turnBanner';
    banner.textContent = v.phase.charAt(0).toUpperCase() + v.phase.slice(1) + '…';
  } else {
    banner.hidden = true;
  }

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

el('sitOut').addEventListener('click', async () => {
  if (!confirm('Get up from the table? The session ends after the current hand.')) return;
  try {
    await postJSON('/api/sitout', { identityKey });
    setStatus('seatStatus', 'You are getting up. The table stops after this hand.', 'ok');
  } catch (e) {
    setStatus('seatStatus', e.message, 'bad');
  }
});
