// Shared by the UI tests: where the test copy is, a guard that keeps them
// away from a real installation, the browser and the output folder.
import { chromium } from "playwright-core";
import { mkdirSync, mkdtempSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { join, sep } from "node:path";

export const APP = (process.env.INDIRGEN_URL ?? "http://127.0.0.1:8898").replace(/\/$/, "");
export const OUT = process.env.UI_OUT ?? mkdtempSync(join(tmpdir(), "indirgen-ui-"));
mkdirSync(OUT, { recursive: true });

// These tests subscribe, download and change settings. They run only on a
// test copy: on this computer, not on the default port 8899, and with its
// data and archive folders outside your home folder (README.md).
export async function guardTestCopy() {
  const url = new URL(APP);
  if (!["127.0.0.1", "localhost"].includes(url.hostname) || url.port === "" || url.port === "8899") {
    throw new Error(`${APP} is not a test copy: use 127.0.0.1 and a port other than 8899`);
  }
  const about = await (await fetch(`${APP}/api/about`)).json();
  const home = homedir() + sep;
  for (const [what, dir] of [["data", about.data], ["archive", about.library]]) {
    if (!dir || (dir + sep).startsWith(home)) {
      throw new Error(`the ${what} folder of ${APP} is ${dir}, inside your home folder: not a test copy`);
    }
  }
  return about;
}

export const launch = () => chromium.launch({ executablePath: process.env.CHROMIUM_PATH });

export const api = async (path, body) => {
  const r = await fetch(APP + path, body === undefined ? {} : {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
  });
  if (!r.ok) throw new Error(`${path}: ${r.status} ${await r.text()}`);
  return r.json();
};

export const pause = ms => new Promise(r => setTimeout(r, ms));

// Collects failed expectations; finish() prints them and sets the exit code.
export function expectations() {
  const problems = [];
  return {
    check: (ok, msg) => { if (!ok) problems.push(msg); },
    add: msg => problems.push(msg),
    finish() {
      console.log(`\nscreenshots: ${OUT}`);
      console.log(problems.length ? `PROBLEMS:\n${problems.join("\n")}` : "no problems");
      process.exit(problems.length ? 1 : 0);
    },
  };
}
