function ConvertTo-NormalizedLineEnding {
    param(
        [AllowEmptyString()]
        [string] $Text
    )

    return $Text.Replace("`r`n", "`n").Replace("`r", "`n")
}

function Test-NormalizedTextEqual {
    param(
        [AllowEmptyString()]
        [string] $Expected,

        [AllowEmptyString()]
        [string] $Actual
    )

    return (ConvertTo-NormalizedLineEnding $Expected) -ceq
        (ConvertTo-NormalizedLineEnding $Actual)
}
