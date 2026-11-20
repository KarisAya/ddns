export default {
  async fetch(request) {
    const url = new URL(request.url);
    const expectedFamily = {
      "/ipv4": 4,
      "/ipv6": 6,
    }[url.pathname];
    if (!expectedFamily) {
      return new Response("Not Found", { status: 404 });
    }

    const ip = request.headers.get("CF-Connecting-IP");
    const isIPv4 = ip && /^\d{1,3}(?:\.\d{1,3}){3}$/.test(ip) && ip.split(".").every((part) => Number(part) <= 255);
    const isIPv6 = ip && ip.includes(":");
    if ((expectedFamily === 4 && !isIPv4) || (expectedFamily === 6 && !isIPv6)) {
      return new Response(`No IPv${expectedFamily} address detected`, {
        status: 503,
        headers: { "Content-Type": "text/plain; charset=utf-8" },
      });
    }
    return new Response(ip, {
      headers: {
        "Content-Type": "text/plain; charset=utf-8",
        "Cache-Control": "no-store",
      },
    });
  },
};
