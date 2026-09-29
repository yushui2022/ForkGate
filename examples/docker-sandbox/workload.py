import json
import os
import sys
import urllib.error
import urllib.request


def print_response(response):
    print(json.dumps({
        "status": response.status,
        "staged_id": response.headers.get("X-ForkGate-Staged", ""),
        "body": response.read().decode("utf-8", errors="replace"),
    }))


def main():
    args = sys.argv[1:]
    direct = bool(args and args[0] == "--direct")
    if direct:
        args = args[1:]
    if len(args) not in (2, 3):
        raise ValueError("usage: workload.py [--direct] METHOD URL [BODY]")
    method, target = args[:2]
    method = method.upper()
    if direct and (method != "GET" or len(args) != 2):
        raise ValueError("--direct permits only GET URL without a body")

    # Ensure proxy-mode requests cannot bypass the gateway via inherited settings.
    os.environ.pop("NO_PROXY", None)
    os.environ.pop("no_proxy", None)
    headers = {
        "Content-Type": "application/json",
        "X-Demo-Branch": os.environ.get("FORKGATE_DEMO_BRANCH", ""),
    }
    if direct:
        proxy_handler = urllib.request.ProxyHandler({})
    else:
        proxy = os.environ["FORKGATE_PROXY"]
        headers["Proxy-Authorization"] = "Bearer " + os.environ["FORKGATE_TOKEN"]
        proxy_handler = urllib.request.ProxyHandler({"http": proxy, "https": proxy})
    request = urllib.request.Request(
        target,
        data=args[2].encode() if len(args) == 3 else None,
        method=method,
        headers=headers,
    )
    opener = urllib.request.build_opener(proxy_handler)
    try:
        with opener.open(request, timeout=10) as response:
            print_response(response)
    except urllib.error.HTTPError as error:
        with error:
            print_response(error)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError) as error:
        print(json.dumps({"error": str(error)}), file=sys.stderr)
        sys.exit(1)
