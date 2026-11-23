import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { parse } from "yaml";

const compose = parse(readFileSync(new URL("../docker-compose.selfhost.yml", import.meta.url), "utf8"));
const workflow = parse(readFileSync(new URL("../.github/workflows/fork-docker.yml", import.meta.url), "utf8"));
const publish = workflow.jobs.publish;
const imageVariables = Object.values(compose.services).flatMap((service) => {
  const image = service.image.match(/^\$\{(MULTICA_\w+_IMAGE):-/);
  return image ? [image[1]] : [];
});
const sha = "a".repeat(40);
const env = Object.fromEntries(Object.entries(publish.env).map(([key, value]) => [
  key, value.replace(/\$\{\{\s*github\.sha\s*\}\}/g, sha),
]));

// The smoke stack can pass with an optional service disabled even when the
// commit tag for that service was never built or published by the fork.
test("every self-hosted image, including optional profiles, is built at the fork commit tag", () => {
  assert.equal(env.MULTICA_IMAGE_TAG, `sha-${sha}`);
  const builds = publish.steps.filter((step) => step.uses?.startsWith("docker/build-push-action@"));
  const builtImages = builds.flatMap((step) => {
    assert.equal(step.with.load, true, "the smoke check must use the built image");
    return step.with.tags.replace(/\$\{\{\s*env\.(\w+)\s*\}\}/g, (_, key) => {
      assert.ok(env[key], `Unknown image environment variable: ${key}`);
      return env[key];
    }).split(/\s+/).filter(Boolean);
  });
  for (const variable of imageVariables) {
    assert.ok(env[variable], `The fork must override ${variable}`);
    assert.ok(builtImages.includes(`${env[variable]}:${env.MULTICA_IMAGE_TAG}`), `${variable} is not built at the shared tag`);
  }
});

test("publication pushes every image required by the self-hosted stack", (t) => {
  const dir = mkdtempSync(join(tmpdir(), "fork-docker-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const pushes = join(dir, "pushes");
  writeFileSync(join(dir, "docker"), '#!/bin/sh\n[ "$1" = push ] || exit 1\nprintf "%s\\n" "$2" >> "$PUSH_LOG"\n', { mode: 0o755 });
  const steps = publish.steps.filter((step) => step.run?.includes("docker push"));
  assert.ok(steps.length > 0, "No image publication command found");
  for (const step of steps) {
    const result = spawnSync("bash", ["--noprofile", "--norc", "-e", "-o", "pipefail", "-c", step.run], {
      env: { ...process.env, ...env, PATH: `${dir}:${process.env.PATH}`, PUSH_LOG: pushes, GITHUB_STEP_SUMMARY: join(dir, "summary") },
      encoding: "utf8",
    });
    assert.equal(result.status, 0, result.stderr);
  }
  const expected = imageVariables.map((variable) => {
    assert.ok(env[variable], `The fork must override ${variable}`);
    return `${env[variable]}:${env.MULTICA_IMAGE_TAG}`;
  });
  assert.deepEqual(readFileSync(pushes, "utf8").trim().split("\n").sort(), expected.sort());
});
