// Standalone mock server for manual debugging: prints base URL and request log.
import { MockGeminiServer } from "./mock-server.js";

const s = new MockGeminiServer();
s.start().then((url) => {
  console.log(url);
  setInterval(() => {
    while (s.requests.length) {
      const r = s.requests.shift()!;
      console.log(`>> ${r.method} ${r.path} ${r.body.slice(0, 200)}`);
    }
  }, 200);
});
