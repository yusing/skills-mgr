mod cli;

use skills_mgr::{
    Error, Harness, SkillsManager,
    cli_support::{catalog, condition, content, invalid_data, io_error, remote, selection},
};

fn main() -> std::process::ExitCode {
    cli::main()
}
