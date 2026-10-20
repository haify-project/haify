"""MkDocs hooks for the Haify site.

The site is built from docs/, but some documentation lives next to what it
describes (deploy/k8s/README.md, deploy/proxmox/README.md, ...). Those files
are pulled in as site pages here, and every relative link in every page is
rewritten from repository terms into site terms: a link to a file that is a
site page points at that page, and a link to anything else in the repository
points at it on GitHub. The documents themselves stay written for someone
reading them in the repository.
"""

import os
import posixpath
import re

from mkdocs.structure.files import File

REPO_URL = "https://github.com/haify-project/haify"
BRANCH = "main"

# Repository path -> site page path, for documents outside docs/.
EXTERNAL = {
    "deploy/k8s/README.md": "kubernetes.md",
    "deploy/proxmox/README.md": "proxmox.md",
    "deploy/libvirt/README.md": "libvirt.md",
    "deploy/openstack/README.md": "openstack.md",
    "deploy/xcp-ng/README.md": "xcp-ng.md",
    "deploy/docker/README.md": "docker.md",
    "deploy/nomad/README.md": "nomad.md",
    "deploy/helm/haify-csi/README.md": "helm.md",
    "deploy/monitoring/README.md": "monitoring.md",
    "README_cn.md": "zh.md",
}

LINK = re.compile(r"(!?\[[^\]]*\]\()([^)\s]+)(\s+\"[^\"]*\")?\)")

_repo_root = None
_site_pages = {}  # repository path -> site path


def on_config(config):
    global _repo_root
    _repo_root = os.path.dirname(os.path.abspath(config["config_file_path"]))
    return config


def on_files(files, config):
    for repo_path, site_path in EXTERNAL.items():
        with open(os.path.join(_repo_root, repo_path), encoding="utf-8") as f:
            content = f.read()
        files.append(File.generated(config, site_path, content=content))
    _site_pages.clear()
    for f in files.documentation_pages():
        _site_pages["docs/" + f.src_uri] = f.src_uri
    for repo_path, site_path in EXTERNAL.items():
        _site_pages[repo_path] = site_path
        _site_pages.pop("docs/" + site_path, None)
    for f in files:
        if not f.is_documentation_page():
            _site_pages.setdefault("docs/" + f.src_uri, f.src_uri)
    return files


def _origin(page):
    for repo_path, site_path in EXTERNAL.items():
        if page.file.src_uri == site_path:
            return repo_path
    return "docs/" + page.file.src_uri


def _rewrite(target, origin, page_site_path):
    if re.match(r"^[a-z][a-z0-9+.-]*:", target) or target.startswith("#"):
        return target
    path, _, anchor = target.partition("#")
    if not path:
        return target
    repo_path = posixpath.normpath(posixpath.join(posixpath.dirname(origin), path))
    suffix = "#" + anchor if anchor else ""
    if repo_path in _site_pages:
        rel = posixpath.relpath(_site_pages[repo_path], posixpath.dirname(page_site_path) or ".")
        return rel + suffix
    kind = "tree" if os.path.isdir(os.path.join(_repo_root, repo_path)) else "blob"
    return f"{REPO_URL}/{kind}/{BRANCH}/{repo_path}{suffix}"


def on_page_markdown(markdown, page, config, files):
    origin = _origin(page)

    def sub(m):
        title = m.group(3) or ""
        return f"{m.group(1)}{_rewrite(m.group(2), origin, page.file.src_uri)}{title})"

    out, in_fence = [], False
    for line in markdown.split("\n"):
        if line.lstrip().startswith(("```", "~~~")):
            in_fence = not in_fence
        out.append(line if in_fence else LINK.sub(sub, line))
    return "\n".join(out)
