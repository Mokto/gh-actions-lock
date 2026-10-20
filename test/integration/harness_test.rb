# frozen_string_literal: true

require "digest"
require "rbconfig"
require ENV.fetch("HARNESS_UNDER_TEST", File.expand_path("harness", __dir__))

# All injected bindings below point only at this test's synthetic repositories.
ENV.keys.grep(/\AGIT_/).each { |key| ENV.delete(key) }
ENV.delete("EMAIL")
real_git, status = Open3.capture2("which", "git")
raise "git is unavailable" unless status.success?
real_git = real_git.strip

def snapshot(*roots)
  roots.to_h do |root|
    files = Dir.glob(File.join(root, "**", "*"), File::FNM_DOTMATCH).select { |path| File.file?(path) }
    [root, files.sort.to_h { |path| [path.delete_prefix(root), [File.stat(path).mode, Digest::SHA256.file(path).hexdigest]] }]
  end
end

original_env = ENV.to_h
cases = %i[prepare captured pty captured_after_prepare pty_after_prepare scenario live].product(%i[directory all])
cases << [:auth_override, :all]
cases << [:pty_auth_override, :all]
cases.select! { |mode, _| mode.to_s == ENV["ISOLATION_CASE"] } if ENV["ISOLATION_CASE"]
raise "no matching isolation cases" if cases.empty?
cases.each do |mode, binding|
  Dir.mktmpdir("harness-isolation-") do |tmp|
    home = File.join(tmp, "home")
    parent = File.join(tmp, "parent")
    bin = File.join(tmp, "bin")
    trace = File.join(tmp, "git-trace")
    FileUtils.mkdir_p([home, bin])
    global = File.join(home, ".gitconfig")
    File.write(global, "[user]\n\tname = Decoy\n\temail = decoy@example.com\n")
    safe_env = original_env.merge(
      "HOME" => home, "GIT_CONFIG_GLOBAL" => global, "GIT_CONFIG_NOSYSTEM" => "1",
      "GH_CONFIG_DIR" => File.join(home, "gh"), "XDG_CONFIG_HOME" => File.join(home, "xdg"),
      "GH_TOKEN" => "synthetic-token", "GITHUB_TOKEN" => "synthetic-fallback",
    )
    ENV.replace(safe_env)
    [["init", "-q", "-b", "main", parent],
     ["-C", parent, "config", "user.name", "Decoy repository"],
     ["-C", parent, "config", "user.email", "repository@example.com"]].each do |args|
      system(real_git, *args, exception: true)
    end
    File.write(File.join(parent, "tracked"), "untouched caller\n")
    system(real_git, "-C", parent, "add", "tracked", exception: true)
    system(real_git, "-C", parent, "commit", "-q", "-m", "caller", exception: true)
    before = snapshot(parent, home)

    File.write(File.join(bin, "git"), <<~RUBY)
      \#!#{RbConfig.ruby}
      require "json"
      unless ENV["ISOLATION_GIT_CHILD"]
        File.open(ENV.fetch("ISOLATION_TRACE"), "a") do |file|
          file.puts JSON.generate(kind: ENV["ISOLATION_KIND"], home: ENV["HOME"],
            bindings: ENV.to_h.select { |key, _| key.start_with?("GIT_") || key == "EMAIL" })
        end
      end
      ENV["ISOLATION_GIT_CHILD"] = "1"
      args = ARGV.map { |arg| arg == "https://github.com/fixture/source.git" ? ENV.fetch("ISOLATION_REMOTE") : arg }
      exec ENV.fetch("ISOLATION_GIT"), *args
    RUBY
    File.chmod(0o700, File.join(bin, "git"))
    probe = File.join(bin, "probe")
    File.write(probe, <<~RUBY)
      \#!#{RbConfig.ruby}
      require "fileutils"
      require "json"
      system({"ISOLATION_KIND" => "cli"}, "git", "config", "user.name", "CLI fixture", exception: true)
      FileUtils.mkdir_p(".github/workflows")
      File.write(".github/workflows/ci.yml", "fixture child changed only its repository\\n")
      system({"ISOLATION_KIND" => "cli"}, "git", "add", "-A", exception: true)
      puts JSON.generate(ENV.to_h.select { |key, _| key.start_with?("GIT_") ||
        %w[EMAIL HOME GH_CONFIG_DIR XDG_CONFIG_HOME GH_TOKEN GITHUB_TOKEN].include?(key) })
    RUBY
    File.chmod(0o700, probe)

    injected = {
      "GIT_DIR" => File.join(parent, ".git"),
      "ISOLATION_TRACE" => trace, "ISOLATION_REMOTE" => parent, "ISOLATION_GIT" => real_git,
      "PATH" => "#{bin}:#{safe_env.fetch("PATH")}",
    }
    if binding == :all
      injected.merge!(
        "GIT_WORK_TREE" => parent, "GIT_COMMON_DIR" => File.join(parent, ".git"),
        "GIT_INDEX_FILE" => File.join(parent, ".git/index"),
        "GIT_OBJECT_DIRECTORY" => File.join(parent, ".git/objects"),
        "GIT_ALTERNATE_OBJECT_DIRECTORIES" => File.join(parent, ".git/objects"),
        "GIT_NAMESPACE" => "decoy", "GIT_CONFIG" => File.join(parent, ".git/config"),
        "GIT_CONFIG_SYSTEM" => File.join(parent, ".git/config"),
        "GIT_SHALLOW_FILE" => File.join(parent, ".git/shallow"),
        "GIT_CONFIG_COUNT" => "1", "GIT_CONFIG_KEY_0" => "core.worktree", "GIT_CONFIG_VALUE_0" => parent,
        "GIT_CONFIG_PARAMETERS" => "'user.name=Injected'",
        "GIT_AUTHOR_NAME" => "Injected", "GIT_AUTHOR_EMAIL" => "injected@example.com",
        "GIT_AUTHOR_DATE" => "@1 +0000",
        "GIT_COMMITTER_NAME" => "Injected", "GIT_COMMITTER_EMAIL" => "injected@example.com",
        "GIT_COMMITTER_DATE" => "@1 +0000",
        "EMAIL" => "injected@example.com",
      )
    end
    ENV.update(injected)
    scenario = ActionsPin::Integration::Scenario.new("isolation")
    scenario.workflow("ci.yml", "name: fixture\n")
    scenario.env(injected.select { |key, _| key.start_with?("GIT_") || key == "EMAIL" }) if binding == :all
    scenario.live_repo("fixture/source") if mode == :live
    auth_override = %i[auth_override pty_auth_override].include?(mode)
    scenario.env("GH_TOKEN" => nil, "GH_CONFIG_DIR" => File.join(home, "override-gh")) if auth_override
    context = nil
    failure = nil
    begin
      if mode == :scenario
        result = scenario.run(probe)
      else
        ENV.replace(safe_env.merge(injected.reject { |key, _| key.start_with?("GIT_") || key == "EMAIL" })) if mode.to_s.end_with?("_after_prepare")
        context = scenario.prepare(probe)
        ENV.update(injected)
        pty = %i[pty pty_after_prepare pty_auth_override].include?(mode)
        result = pty ? context.run_pty : context.run_captured unless mode == :prepare
      end
      if result
        raise "child exited #{result.exit_code}: #{result.stderr}" unless result.success?
        child = JSON.parse(result.stdout.strip)
        raise "CLI inherited Git bindings" unless child.keys.grep(/\AGIT_|^EMAIL$/).empty?
        %w[HOME XDG_CONFIG_HOME GITHUB_TOKEN].each do |key|
          raise "CLI authentication context changed: #{key}" unless child[key] == safe_env[key]
        end
        expected_token = auth_override ? nil : safe_env["GH_TOKEN"]
        expected_config = auth_override ? File.join(home, "override-gh") : safe_env["GH_CONFIG_DIR"]
        raise "CLI token override changed" unless child["GH_TOKEN"] == expected_token
        raise "CLI config lookup changed" unless child["GH_CONFIG_DIR"] == expected_config
      end
      records = File.readlines(trace).map { |line| JSON.parse(line) }
      fixture_records = records.reject { |record| record["kind"] == "cli" }
      raise "no fixture Git commands recorded" if fixture_records.empty?
      fixture_records.each do |record|
        raise "fixture Git inherited caller HOME" if record["home"] == home
        expected = { "GIT_CONFIG_GLOBAL" => File.join(record.fetch("home"), ".gitconfig"), "GIT_CONFIG_NOSYSTEM" => "1" }
        raise "fixture Git inherited bindings: #{record["bindings"].inspect}" unless record["bindings"] == expected
      end
    rescue StandardError => error
      failure = error
    ensure
      context&.teardown
      ENV.replace(safe_env)
      raise "#{mode}/#{binding}: caller config, index, refs, objects or tracked bytes changed" unless snapshot(parent, home) == before
    end
    raise "#{mode}/#{binding}: #{failure.message}" if failure
    puts "#{mode}/#{binding}: isolated"
  end
end
ENV.replace(original_env)
puts "Ruby fixture and CLI isolation: #{cases.length} cases passed"
