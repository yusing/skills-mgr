# Install with: skills-mgr completion fish > ~/.config/fish/completions/skills-mgr.fish
function __skills_mgr_command
    set -l words (commandline -opc)
    test (count $words) -ge 2; and contains -- $words[2] $argv
end

complete -c skills-mgr -n __fish_use_subcommand -f -a 'help info adopt list sync install get inspect check set edit run completion'
complete -c skills-mgr -n __fish_use_subcommand -s g -d 'Open global selection'
complete -c skills-mgr -n __fish_use_subcommand -l help -d 'Show usage'

complete -c skills-mgr -n '__skills_mgr_command completion; and test (__fish_number_of_cmd_args_wo_opts) -eq 2' -f -a fish -d 'Print fish completions'
complete -c skills-mgr -n '__skills_mgr_command list get run; and test (__fish_number_of_cmd_args_wo_opts) -eq 2' -l claude -d 'Select Claude harness'
complete -c skills-mgr -n '__skills_mgr_command list get run; and test (__fish_number_of_cmd_args_wo_opts) -eq 2' -l grok -d 'Select Grok harness'
complete -c skills-mgr -n '__skills_mgr_command list get run; and test (__fish_number_of_cmd_args_wo_opts) -eq 2' -l codex -d 'Select Codex harness'
complete -c skills-mgr -n '__skills_mgr_command install set edit; and test (__fish_number_of_cmd_args_wo_opts) -eq 2' -s g -d 'Use global selection'
complete -c skills-mgr -n '__skills_mgr_command install; and test (__fish_number_of_cmd_args_wo_opts) -eq 2' -l exclude -r -d 'Exclude a file pattern'
complete -c skills-mgr -n '__skills_mgr_command edit; and test (__fish_number_of_cmd_args_wo_opts) -eq 3' -l file -r -F -d 'Read skill from a file'
complete -c skills-mgr -n '__skills_mgr_command edit' -l expect-sha256 -r -f -d 'Require the current SHA256'
complete -c skills-mgr -n '__skills_mgr_command set; and test (__fish_number_of_cmd_args_wo_opts) -eq 3' -f -a 'true false inherit'

complete -c skills-mgr -n '__skills_mgr_command get; and test (__fish_number_of_cmd_args_wo_opts) -eq 2' -f -a '(skills-mgr __complete-skills enabled 2>/dev/null)'
complete -c skills-mgr -n '__skills_mgr_command run; and test (__fish_number_of_cmd_args_wo_opts) -eq 2' -f -a '(skills-mgr __complete-skills enabled 2>/dev/null | string replace -r "\$" /)'
complete -c skills-mgr -n '__skills_mgr_command check' -f -a '(skills-mgr __complete-skills enabled 2>/dev/null)'
complete -c skills-mgr -n '__skills_mgr_command inspect set edit; and test (__fish_number_of_cmd_args_wo_opts) -eq 2' -f -a '(skills-mgr __complete-skills all 2>/dev/null)'
