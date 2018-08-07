# Copyright 2018 The Bazel Authors. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

load(
    "@io_bazel_rules_go//go:def.bzl",
    "GoArchive",
    "go_context",
)
load(
    "@io_bazel_rules_go//go/private:providers.bzl",
    "GoAspectProviders",
    "GoStdLib",
    "GoTest",
)

def _archive(v):
    return "{}={}={}=".format(v.data.label, v.data.importpath, v.data.importmap)

def _archive_export(v):
    return "{}={}={}={}".format(v.data.label, v.data.importpath, v.data.importmap, v.data.file.path)

_GoPackages = provider(
    fields = {
        "files": "depset of package json files",
        "transitive": "transitive component of files (list of depsets)",
    },
)

def _gopackagesdriver_aspect_impl(target, ctx):
    if all([p not in target for p in [GoAspectProviders, GoTest, GoStdLib, GoArchive]]):
        # Not a Go target.
        return []

    go = go_context(ctx, ctx.rule.attr)
    if GoAspectProviders in target:
        # TODO(jayconrod): handle tests and stdlib
        archive = target[GoAspectProviders].archive
        package_files = [_build_package_file(go, ctx, archive)]
    elif GoStdLib in target:
        package_files = _build_stdlib_package_files(go, target, ctx)
    elif GoTest in target:
        if ctx.attr._test:
            testmain = target[GoArchive]
            internal = target[GoTest].internal
            external = target[GoTest].external
            package_files = [
                _build_package_file(go, ctx, testmain, suffix = "testmain"),
                _build_package_file(go, ctx, internal, suffix = "test_internal", testfilter = "exclude"),
                _build_package_file(go, ctx, external, suffix = "test_external", testfilter = "only"),
            ]
        else:
            package_files = []
    else:
        archive = target[GoArchive]
        package_files = [_build_package_file(go, ctx, archive)]

    if ctx.attr._deps:
        transitive = [d[_GoPackages].files for d in getattr(ctx.rule.attr, "deps", [])]
        if ctx.attr._test:
            for e in getattr(ctx.rule.attr, "embed", []):
                transitive.append(e[_GoPackages].files)
        else:
            for e in getattr(ctx.rule.attr, "embed", []):
                transitive.extend(e[_GoPackages].transitive)
        if hasattr(ctx.rule.attr, "_stdlib"):
            transitive.append(ctx.rule.attr._stdlib[_GoPackages].files)
        outputs = depset(direct = package_files, transitive = transitive)
        return [
            _GoPackages(
                files = outputs,
                transitive = transitive,
            ),
            OutputGroupInfo(gopackagesdriver = outputs),
        ]
    else:
        return [OutputGroupInfo(gopackagesdriver = package_files)]

def _define_aspect(deps, export, test):
    attr_aspects = []
    if deps:
        attr_aspects.extend(["deps", "embed", "_stdlib"])
    attrs = {
        "_deps": attr.bool(default = deps),
        "_test": attr.bool(default = test),
        "_export": attr.bool(default = export),
        "_package": attr.label(
            default = "@io_bazel_rules_go//go/tools/builders:package",
            executable = True,
            cfg = "host",
        ),
    }
    return aspect(
        _gopackagesdriver_aspect_impl,
        attr_aspects = attr_aspects,
        attrs = attrs,
        toolchains = ["@io_bazel_rules_go//go:toolchain"],
    )

gopackagesdriver_aspect = _define_aspect(deps = False, export = False, test = False)
gopackagesdriver_test_aspect = _define_aspect(deps = False, export = False, test = True)
gopackagesdriver_export_aspect = _define_aspect(deps = False, export = True, test = False)
gopackagesdriver_export_test_aspect = _define_aspect(deps = False, export = True, test = True)
gopackagesdriver_deps_aspect = _define_aspect(deps = True, export = False, test = False)
gopackagesdriver_deps_test_aspect = _define_aspect(deps = True, export = False, test = True)
gopackagesdriver_deps_export_aspect = _define_aspect(deps = True, export = True, test = False)
gopackagesdriver_deps_export_test_aspect = _define_aspect(deps = True, export = True, test = True)

def _build_package_file(go, ctx, archive, testfilter = None, suffix = None):
    pkg_file = go.declare_file(go, name = archive.data.name, ext = ".json")
    args = go.builder_args(go)
    inputs = []
    id = "{}%{}".format(archive.data.label, suffix) if suffix else archive.data.label
    args.add("-id", id)
    args.add("-importpath", archive.data.importpath)
    args.add("-importmap", archive.data.importmap)
    if ctx.attr._export:
        args.add("-file", archive.data.file)
        inputs.append(archive.data.file)
    go_srcs = [src for src in archive.data.srcs if src.extension == "go"]
    args.add_all(go_srcs, before_each = "-go_src")
    other_srcs = [src for src in archive.data.srcs if src.extension != "go"]
    args.add_all(other_srcs, before_each = "-other_src")
    inputs.extend(archive.data.srcs)
    orig_srcs = [src for src in archive.data.orig_srcs if src.extension == "go"]
    args.add_all(orig_srcs, before_each = "-orig_src")
    inputs.extend(orig_srcs)
    if testfilter:
        args.add("-testfilter", testfilter)
    if ctx.attr._export:
        args.add_all(archive.direct, before_each = "-arc", map_each = _archive_export)
        inputs.extend([dep.data.file for dep in archive.direct])
    else:
        args.add_all(archive.direct, before_each = "-arc", map_each = _archive)
    args.add("-o", pkg_file)
    go.actions.run(
        inputs = inputs,
        outputs = [pkg_file],
        mnemonic = "GoPackage",
        executable = ctx.executable._package,
        arguments = ["pkg", args],
        env = go.env,
    )
    return pkg_file

def _build_stdlib_package_files(go, target, ctx):
    pkg_files = go.declare_directory(go, name = target.label.name)
    inputs = target[GoStdLib].libs + [go.go]
    args = go.builder_args(go)
    args.add("-go", go.go)
    args.add("-o", pkg_files)
    outputs = [pkg_files]
    env = go.env
    env.update({
        "CC": go.cgo_tools.compiler_executable,
        "CGO_CPPFLAGS": " ".join(go.cgo_tools.compiler_options),
        "CGO_CFLAGS": " ".join(go.cgo_tools.c_options),
        "CGO_LDFLAGS": " ".join(go.cgo_tools.linker_options),
    })
    go.actions.run(
        inputs = inputs,
        outputs = outputs,
        mnemonic = "GoPackage",
        executable = ctx.executable._package,
        arguments = ["stdlib", args],
        env = go.env,
    )
    return [pkg_files]
