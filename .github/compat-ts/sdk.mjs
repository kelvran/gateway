// The official @anthropic-ai/sdk against the gateway's Anthropic routes, run by
// gateway/compat/ts_leg_test.go (build tag compat_ts) with the gateway's
// address and credential in the environment. Every expected value is a byte of
// a golden the Go harness's mock replays (gateway/compat/testdata/): an
// anthropic deployment relays Anthropic's own response, so the SDK must see
// Anthropic's own id, model and usage.
//
// Prints one JSON line per check and a final summary; exits 1 when any check
// failed, 2 when the environment is not set (the leg fails, never skips).
import Anthropic from "@anthropic-ai/sdk";

const url = process.env.KELVRAN_COMPAT_URL;
const credential = process.env.KELVRAN_COMPAT_CREDENTIAL;
const model = process.env.KELVRAN_COMPAT_MODEL || "claude-sys";
if (!url || !credential) {
  console.error("KELVRAN_COMPAT_URL and KELVRAN_COMPAT_CREDENTIAL must be set: the TypeScript leg runs only against a live gateway");
  process.exit(2);
}

const EXPECTED_TEXT = "It's 72°F and sunny in Boston.";
const EXPECTED_STREAM_TEXT = "The sky is blue today.";
// Distinct per check: the gateway's response cache is on, as it is for every
// operator, and an identical body would be answered from the cache.
const question = (tag) => [{ role: "user", content: `What's the weather in Boston? [ts ${tag}]` }];
const textOf = (msg) => msg.content.filter((b) => b.type === "text").map((b) => b.text).join("");

const results = [];
async function check(name, fn) {
  try {
    const detail = await fn();
    results.push({ check: name, ok: true, detail });
  } catch (err) {
    results.push({ check: name, ok: false, error: String(err && err.message ? err.message : err) });
  }
  console.log(JSON.stringify(results[results.length - 1]));
}
const expect = (cond, what) => {
  if (!cond) throw new Error(what);
};

const client = new Anthropic({ baseURL: url, apiKey: credential, maxRetries: 0 });

await check("buffered message is Anthropic's own bytes", async () => {
  const msg = await client.messages.create({ model, max_tokens: 64, messages: question("buffered") });
  expect(msg.id === "msg_01XFDUDYJgAACzvnptvVoYEL", `id ${msg.id}`);
  expect(msg.model === "claude-opus-4-20250514", `model ${msg.model}`);
  expect(textOf(msg) === EXPECTED_TEXT, `text ${textOf(msg)}`);
  expect(msg.stop_reason === "end_turn", `stop_reason ${msg.stop_reason}`);
  expect(msg.usage.input_tokens === 58 && msg.usage.output_tokens === 12, `usage ${JSON.stringify(msg.usage)}`);
  return { id: msg.id, model: msg.model };
});

await check("stream accumulates the relayed frames", async () => {
  const final = await client.messages.stream({ model, max_tokens: 64, messages: question("stream") }).finalMessage();
  expect(textOf(final) === EXPECTED_STREAM_TEXT, `text ${textOf(final)}`);
  expect(final.stop_reason === "end_turn", `stop_reason ${final.stop_reason}`);
  expect(final.usage.output_tokens === 9, `output_tokens ${final.usage.output_tokens}`);
  expect(final.id === "msg_01Text0nly000000000000", `id ${final.id}`);
  return { id: final.id };
});

await check("count_tokens is the deployment's own count", async () => {
  const count = await client.messages.countTokens({ model, messages: question("count") });
  expect(count.input_tokens === 2095, `input_tokens ${count.input_tokens}`);
  return { input_tokens: count.input_tokens };
});

await check("models.list names the model", async () => {
  const page = await client.models.list();
  const ids = page.data.map((m) => m.id);
  expect(ids.includes(model), `ids ${ids.join(",")}`);
  expect(page.data.every((m) => m.type === "model"), "a model without type: model");
  return { ids };
});

await check("error envelope carries a code", async () => {
  try {
    await client.messages.create({ model: "no-such-model", max_tokens: 64, messages: question("error") });
  } catch (err) {
    expect(err instanceof Anthropic.BadRequestError, `error class ${err && err.constructor && err.constructor.name}`);
    expect(err.status === 400, `status ${err.status}`);
    const body = err.error;
    expect(body && body.type === "error" && body.error && body.error.type === "invalid_request_error", `envelope ${JSON.stringify(body)}`);
    expect(body.error.code === "model_not_found", `code ${body.error.code}`);
    return { code: body.error.code };
  }
  throw new Error("an unknown model was accepted");
});

await check("bearer credential form is accepted", async () => {
  const bearer = new Anthropic({ baseURL: url, authToken: credential, maxRetries: 0 });
  const msg = await bearer.messages.create({ model, max_tokens: 64, messages: question("bearer") });
  expect(textOf(msg) === EXPECTED_TEXT, `text ${textOf(msg)}`);
  return { ok: true };
});

await check("wrong credential is 401", async () => {
  const wrong = new Anthropic({ baseURL: url, apiKey: "not-" + credential, maxRetries: 0 });
  try {
    await wrong.messages.create({ model, max_tokens: 64, messages: question("wrong") });
  } catch (err) {
    expect(err instanceof Anthropic.AuthenticationError && err.status === 401, `got ${err && err.status}`);
    return { status: err.status };
  }
  throw new Error("a wrong credential was accepted");
});

const failed = results.filter((r) => !r.ok).length;
console.log(JSON.stringify({ summary: true, passed: results.length - failed, failed, sdk: "@anthropic-ai/sdk" }));
process.exit(failed === 0 ? 0 : 1);
