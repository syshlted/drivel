# bash completion for drivel                               -*- shell-script -*-
#
# Install:
#   # system-wide (Debian/Ubuntu/Fedora):
#   sudo cp completions/drivel.bash /usr/share/bash-completion/completions/drivel
#   # or per-user, sourced from ~/.bashrc:
#   source /path/to/drivel/completions/drivel.bash
#
# Requires the bash-completion package for the _init_completion helper; a
# minimal fallback is used when it is absent.

_drivel() {
    local cur prev words cword
    if declare -F _init_completion >/dev/null 2>&1; then
        _init_completion -n : || return
    else
        # Minimal fallback when bash-completion isn't loaded.
        COMPREPLY=()
        cur="${COMP_WORDS[COMP_CWORD]}"
        prev="${COMP_WORDS[COMP_CWORD-1]}"
        words=("${COMP_WORDS[@]}")
        cword=$COMP_CWORD
    fi

    local subcommands="login mount help"

    # Flags that take a path/dir argument -> complete filenames.
    local file_flags="-mount -data -credentials -token -state -index -config"
    # Flags that take a free-form value -> no filename completion.
    local value_flags="-drive-root -drive-sweep-mode -client-id -client-secret -project-id -scope -port -account -sweep-interval"

    # Determine the active subcommand (first non-flag word after "drivel").
    local sub="" i
    for (( i=1; i < cword; i++ )); do
        case "${words[i]}" in
            -*) ;;
            *) sub="${words[i]}"; break ;;
        esac
    done
    # A leading flag (e.g. `drivel -mount ...`) implies the default mount command.
    if [[ -z $sub && ${words[1]} == -* ]]; then
        sub="mount"
    fi

    # Value completion based on the previous flag.
    case "$prev" in
        -mount|-data)
            _filedir -d
            return
            ;;
        -credentials|-token|-state|-index|-config)
            _filedir
            return
            ;;
        -scope)
            COMPREPLY=( $(compgen -W "drive drive.readonly drive.file" -- "$cur") )
            return
            ;;
        -drive-sweep-mode)
            COMPREPLY=( $(compgen -W "auto flat scoped" -- "$cur") ); return ;;
        -drive-root|-client-id|-client-secret|-project-id|-port|-account|-sweep-interval)
            # Opaque values; nothing to suggest.
            return
            ;;
    esac

    # Complete the subcommand slot.
    if [[ -z $sub ]]; then
        if [[ $cur == -* ]]; then
            COMPREPLY=( $(compgen -W "$file_flags $value_flags -lazy -xattr -resync -materialize -max-deletes -debug -open -h -help" -- "$cur") )
        else
            COMPREPLY=( $(compgen -W "$subcommands" -- "$cur") )
        fi
        return
    fi

    # Flag completion within a subcommand.
    local flags=""
    case "$sub" in
        mount) flags="-config -mount -data -credentials -token -state -index -drive-root -drive-sweep-mode -lazy -xattr -resync -materialize -max-deletes -sweep-interval -debug -pprof -h -help" ;;
        login) flags="-account -config -credentials -token -client-id -client-secret -project-id -scope -port -open -h -help" ;;
        help)  return ;;
    esac
    if [[ $cur == -* ]]; then
        COMPREPLY=( $(compgen -W "$flags" -- "$cur") )
    fi
}

complete -F _drivel drivel
