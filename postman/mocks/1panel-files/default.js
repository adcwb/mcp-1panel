// Local contract-based stand-in. It never touches the real filesystem or panel.
// Only the two routes exercised by the simulation are implemented.
const http = require("http");
const crypto = require("crypto");

http.createServer(async (req, res) => {
    const reply = (status, code, data) => {
        res.writeHead(status, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ code, data }));
    };
    const timestamp = req.headers["1panel-timestamp"];
    const expected = crypto.createHash("md5").update("1paneltest-key" + timestamp).digest("hex");
    if (!timestamp || req.headers["1panel-token"] !== expected || req.headers.currentnode !== "local") {
        return reply(401, 401, null);
    }
    const body = req.body;
    if (req.method !== "POST" || !body || typeof body.path !== "string") {
        return reply(501, 501, null);
    }
    const key = "1panel-files:" + body.path;
    // @endpoint POST /api/v2/files
    if (req.url === "/api/v2/files") {
        const writes = (await pm.state.get(key)) || 0;
        await pm.state.set(key, writes + 1);
        // Deliberately fail after committing, modeling an uncertain write.
        return body.path.endsWith("uncertain") ? reply(500, 500, null) : reply(200, 200, null);
    }
    // @endpoint POST /api/v2/files/check
    if (req.url === "/api/v2/files/check") {
        return reply(200, 200, !!(await pm.state.get(key)));
    }
    return reply(501, 501, null);
}).listen(process.env.PORT || 47862);
