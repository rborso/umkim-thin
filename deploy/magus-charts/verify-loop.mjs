// The GM loop, as a script rather than an inline `node -e`: the nested quoting of
// an ESM import inside kubectl exec is what made the inline version return empty.
//
// Engine rolls -> model narrates. Prints one line; verify.sh asserts on it.
import fs from 'node:fs';
import { setConfigFilePath } from '/home/node/app/src/util.js';

const ROOT = '/home/node/app/data/default-user';
const oai = JSON.parse(fs.readFileSync(`${ROOT}/settings.json`, 'utf8')).oai_settings || {};
setConfigFilePath('/home/node/app/config/config.yaml');
const { SecretManager, SECRET_KEYS } = await import('/home/node/app/src/endpoints/secrets.js');
const key = new SecretManager({ root: ROOT }).readSecret(SECRET_KEYS.CUSTOM);

const mcp = async (method, params) => {
  const r = await fetch('http://sillytavern-combat:3921/mcp', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method, params }),
  });
  const raw = await r.text();
  return JSON.parse((raw.split('\n').find(l => l.startsWith('data: '))?.slice(6) ?? raw)).result;
};

// The engine has to have someone to roll for. Seed a minimal sheet if needed.
let roll = JSON.parse((await mcp('tools/call', {
  name: 'check_skill', arguments: { character_id: 'ci-teszt', skill: 'zarzarolas' },
})).content[0].text);
if (!roll.ok) {
  await mcp('tools/call', {
    name: 'sheet_import',
    arguments: {
      mode: 'create_only',
      sheet: {
        id: 'ci-teszt', name: 'Teszt János',
        attrs: { ugyesseg: 15, ero: 12, gyorsasag: 14, egeszseg: 13, karizma: 11, asztral: 10 },
        skills: [{ id: 'zarzarolas', rank: 3 }],
      },
    },
  });
  roll = JSON.parse((await mcp('tools/call', {
    name: 'check_skill', arguments: { character_id: 'ci-teszt', skill: 'zarzarolas' },
  })).content[0].text);
}
if (!roll.ok) { console.log('ROLL_FAILED'); process.exit(0); }

const res = await fetch(`${String(oai.custom_url).replace(/\/$/, '')}/chat/completions`, {
  method: 'POST',
  headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json', 'User-Agent': 'st-magus/1.0' },
  body: JSON.stringify({
    model: oai.custom_model,
    max_tokens: 900,
    messages: [
      { role: 'system', content: 'Magyar mesélő vagy. A kockadobás eredményét kaptad, ne dobj újra. Egy mondatban meséld el.' },
      { role: 'user', content: `Zárnyitás. k10=${roll.roll.raw}, siker: ${roll.success}, margin: ${roll.margin}` },
    ],
  }),
});
const j = await res.json();
const choice = j.choices?.[0];
const content = (choice?.message?.content || '').trim();
// Distinguish "the budget went on reasoning" from "the model returned nothing".
if (!content) {
  console.log(choice?.finish_reason === 'length' ? 'TRUNCATED:token-budget-too-small-for-reasoning' : 'EMPTY');
  process.exit(0);
}
console.log(content.slice(0, 80));
